// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/repo"
)

// newDeployTestRouter 仅挂载发布相关路由，避免 buildRouter 的重依赖。
func newDeployTestRouter(t *testing.T) (*gin.Engine, *ent.Client) {
	t.Helper()
	client, err := repo.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(context.Background()))
	t.Cleanup(func() { client.Close() })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	dh := NewDeployHandler(deploy.New(client))
	g := r.Group("/api/v1/change-orders")
	{
		g.POST("", dh.Create)
		g.GET("", dh.List)
		g.GET("/:id", dh.Get)
		g.POST("/:id/submit", dh.Submit)
		g.POST("/:id/approve", dh.Approve)
		g.POST("/:id/reject", dh.Reject)
		g.POST("/:id/cancel", dh.Cancel)
	}
	return r, client
}

type orderEnvelope struct {
	Code int `json:"code"`
	Data struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	} `json:"data"`
}

func decodeOrder(t *testing.T, body *bytes.Buffer) orderEnvelope {
	t.Helper()
	var env orderEnvelope
	require.NoError(t, json.Unmarshal(body.Bytes(), &env))
	return env
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestDeployHandler_Lifecycle：创建 → 提交 → 批准 → 详情状态校验。
func TestDeployHandler_Lifecycle(t *testing.T) {
	r, _ := newDeployTestRouter(t)

	// 创建 draft
	w := doJSON(t, r, http.MethodPost, "/api/v1/change-orders", map[string]any{
		"title":        "灰度发布 nginx.conf",
		"type":         "config",
		"target_nodes": []int{1, 2},
		"created_by":   "tianhao",
	})
	require.Equal(t, http.StatusOK, w.Code)
	created := decodeOrder(t, w.Body)
	require.Equal(t, 0, created.Code)
	require.Equal(t, "draft", created.Data.Status)
	id := created.Data.ID

	// 提交 → pending_approval
	w = doJSON(t, r, http.MethodPost, "/api/v1/change-orders/"+strconv.Itoa(id)+"/submit", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "pending_approval", decodeOrder(t, w.Body).Data.Status)

	// 批准 → pending
	w = doJSON(t, r, http.MethodPost, "/api/v1/change-orders/"+strconv.Itoa(id)+"/approve", map[string]any{"approved_by": "admin"})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "pending", decodeOrder(t, w.Body).Data.Status)

	// 详情校验
	req := httptest.NewRequest(http.MethodGet, "/api/v1/change-orders/"+strconv.Itoa(id), nil)
	gw := httptest.NewRecorder()
	r.ServeHTTP(gw, req)
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, "pending", decodeOrder(t, gw.Body).Data.Status)
}
