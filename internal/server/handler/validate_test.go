// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// fakeValidator 记录调用参数并返回预设结果/错误，便于断言 HTTP 层映射。
type fakeValidator struct {
	lastNode int
	lastTask *agentv1.ValidateTask
	res      *agentv1.ValidateResult
	err      error
}

func (f *fakeValidator) ValidateConfig(ctx context.Context, nodeID int, task *agentv1.ValidateTask) (*agentv1.ValidateResult, error) {
	f.lastNode = nodeID
	f.lastTask = task
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func newValidateRouter(fv *fakeValidator) (*gin.Engine, *fakeValidator) {
	gin.SetMode(gin.TestMode)
	h := NewValidateHandler(fv)
	r := gin.New()
	r.POST("/api/v1/configs/validate", h.Validate)
	return r, fv
}

func postValidate(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/configs/validate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestValidateHandler_OK(t *testing.T) {
	r, fv := newValidateRouter(&fakeValidator{})
	fv.res = &agentv1.ValidateResult{Ok: true, Raw: "syntax is ok"}
	w := postValidate(t, r, `{"node_id":1,"conf_path":"nginx.conf","files":[{"path":"nginx.conf","content":"events{}"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["code"].(float64) != 0 {
		t.Fatalf("code 期望 0，实际 %v", out["code"])
	}
	if len(fv.lastTask.GetFiles()) != 1 {
		t.Fatalf("文件未透传到校验器: %+v", fv.lastTask.GetFiles())
	}
}

func TestValidateHandler_SyntaxError(t *testing.T) {
	r, fv := newValidateRouter(&fakeValidator{})
	fv.res = &agentv1.ValidateResult{
		Ok:  false,
		Raw: `nginx: [emerg] unknown directive "x" in /etc/nginx/conf.d/api.conf:12`,
		Errors: []*agentv1.NginxError{
			{Level: "emerg", Message: `unknown directive "x"`, File: "/etc/nginx/conf.d/api.conf", Line: 12},
		},
	}
	w := postValidate(t, r, `{"node_id":1,"conf_path":"nginx.conf","files":[{"path":"nginx.conf","content":"x"}]}`)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("期望 412，实际 %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if int(out["code"].(float64)) != int(apperr.CodePrecondition) {
		t.Fatalf("code 期望 %d(4012)，实际 %v", apperr.CodePrecondition, out["code"])
	}
	if out["detail"].(string) == "" {
		t.Fatal("detail 不应为空（应含 nginx 原始输出）")
	}
	data, ok := out["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("结构化错误未随 data 返回: %v", out["data"])
	}
}

func TestValidateHandler_AgentOffline(t *testing.T) {
	r, fv := newValidateRouter(&fakeValidator{})
	fv.err = apperr.New(apperr.CodeUnavailable, "目标 Agent 未在线，无法执行校验")
	w := postValidate(t, r, `{"node_id":1,"conf_path":"nginx.conf","files":[{"path":"nginx.conf","content":"x"}]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实际 %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if int(out["code"].(float64)) != int(apperr.CodeUnavailable) {
		t.Fatalf("code 期望 %d(4103)，实际 %v", apperr.CodeUnavailable, out["code"])
	}
}

func TestValidateHandler_BadInput(t *testing.T) {
	r, _ := newValidateRouter(&fakeValidator{})
	// node_id 缺失 + 无文件：参数非法
	w := postValidate(t, r, `{"node_id":0,"files":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d: %s", w.Code, w.Body.String())
	}
}
