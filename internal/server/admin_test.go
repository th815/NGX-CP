// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// 首跑令牌获取端点测试：锁定「未确认可取一次 / 确认后 410 / 确认需有效令牌 / 空令牌 409」。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/th/ngxcp/internal/config"
	"github.com/gin-gonic/gin"
)

// testAuth 是仅用于测试的鉴权中间件：Bearer 等于给定令牌即通过。
func testAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		got := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		if token != "" && got != token {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 4003, "message": "未授权"})
			return
		}
		c.Next()
	}
}

func newAdminEngine(t *testing.T, ackFile, token string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{AuthAdminToken: token, AuthAdminTokenAckFile: ackFile}
	r := gin.New()
	registerAdmin(r, cfg, testAuth(token))
	return r
}

func doReq(r *gin.Engine, method, path, auth string) (*httptest.ResponseRecorder, map[string]any) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	r.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w, body
}

func TestSetupTokenRevealAndLock(t *testing.T) {
	// 1) 未确认：免鉴权可取一次令牌。
	r := newAdminEngine(t, t.TempDir()+"/missing.ack", "secret-token")
	w, body := doReq(r, http.MethodGet, "/api/v1/admin/setup-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("未确认 setup-token 应 200，得 %d: %s", w.Code, w.Body.String())
	}
	data, _ := body["data"].(map[string]any)
	if data["token"] != "secret-token" {
		t.Fatalf("setup-token 返回令牌 = %v，want secret-token", data["token"])
	}

	// 2) 无令牌调用确认 → 401。
	w, _ = doReq(r, http.MethodPost, "/api/v1/admin/setup-acknowledge", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌确认应 401，得 %d", w.Code)
	}

	// 3) 凭有效令牌确认 → 200，且标记文件被创建。
	ackFile := t.TempDir() + "/created.ack"
	r2 := newAdminEngine(t, ackFile, "secret-token")
	w, _ = doReq(r2, http.MethodPost, "/api/v1/admin/setup-acknowledge", "secret-token")
	if w.Code != http.StatusOK {
		t.Fatalf("确认应 200，得 %d: %s", w.Code, w.Body.String())
	}

	// 4) 确认后 setup-token → 410，不再泄露。
	w, _ = doReq(r2, http.MethodGet, "/api/v1/admin/setup-token", "")
	if w.Code != http.StatusGone {
		t.Fatalf("确认后 setup-token 应 410，得 %d", w.Code)
	}
}

func TestSetupTokenNoConfiguredToken(t *testing.T) {
	// 未配置 auth_admin_token（开发态留空）→ 409。
	r := newAdminEngine(t, t.TempDir()+"/missing.ack", "")
	w, _ := doReq(r, http.MethodGet, "/api/v1/admin/setup-token", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("空令牌 setup-token 应 409，得 %d", w.Code)
	}
}
