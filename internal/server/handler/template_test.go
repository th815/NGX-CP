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
	"github.com/stretchr/testify/require"
	configstore "github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/repo"
)

func newTemplateRouter(t *testing.T) (*gin.Engine, *configstore.TemplateService) {
	t.Helper()
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(ctx))
	t.Cleanup(func() { client.Close() })

	svc := configstore.NewTemplateService(client)

	// 集群 + 节点 + 三级变量（global 级 cluster=prod；node 级 port 覆盖）。
	cl, err := client.Cluster.Create().SetName("prod").Save(ctx)
	require.NoError(t, err)
	n, err := client.Node.Create().SetName("rs-01").SetRole("real_server").
		SetStatus("online").SetAddress("10.0.0.11").SetCluster(cl).Save(ctx)
	require.NoError(t, err)
	require.NoError(t, svc.SetVariable(ctx, configstore.ScopeGlobal, 0, "cluster", "prod", false))
	require.NoError(t, svc.SetVariable(ctx, configstore.ScopeNode, n.ID, "port", "8080", false))
	require.NoError(t, svc.SetVariable(ctx, configstore.ScopeGlobal, 0, "db_pass", "s3cr3t", true))

	_, err = svc.CreateTemplate(ctx, "upstream",
		"upstream {{ .cluster }}_b {\n    server 127.0.0.1:{{ .port }};\n}\n",
		"conf.d/upstream-{cluster}.conf", []string{"cluster", "port"})
	require.NoError(t, err)

	h := NewTemplateHandler(svc)
	r := gin.New()
	ts := r.Group("/api/v1/templates")
	{
		ts.GET("", h.ListTemplates)
		ts.GET("/:id", h.GetTemplate)
		ts.POST("/:id/render", h.RenderTemplate)
	}
	vs := r.Group("/api/v1/variables")
	{
		vs.GET("", h.ListVariables)
		vs.POST("", h.SetVariable)
	}
	return r, svc
}

func TestTemplateHandler_RenderAndMask(t *testing.T) {
	r, svc := newTemplateRouter(t)
	_ = svc

	// 1) 列表
	req := httptest.NewRequest(http.MethodGet, "/api/v1/templates", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var list map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Equal(t, float64(1), list["total"])

	// 2) 渲染节点 1（从 DB 查 node id）
	// 取第一个节点 id：通过变量解析服务间接确认已存在，这里直接渲染 node_id=1。
	body := `{"node_ids":[1]}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/templates/1/render", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	var out2 map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &out2))
	data2 := out2["data"].(map[string]any)
	rendered := data2["1"].(string)
	require.Contains(t, rendered, "upstream prod_b")
	require.Contains(t, rendered, "127.0.0.1:8080")

	// 3) 变量列表：secret 值被打码
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/variables", nil)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	require.Equal(t, http.StatusOK, w3.Code, w3.Body.String())
	var out3 map[string]any
	require.NoError(t, json.Unmarshal(w3.Body.Bytes(), &out3))
	items := out3["data"].([]any)
	foundMasked := false
	for _, it := range items {
		v := it.(map[string]any)
		if v["key"] == "db_pass" {
			require.Equal(t, "******", v["value"], "secret 变量必须打码")
			require.Equal(t, true, v["secret"])
			foundMasked = true
		}
	}
	require.True(t, foundMasked, "应列出 db_pass 且打码")

	// 4) 写入新变量
	body4 := `{"scope":"global","target_id":0,"key":"new_k","value":"v1","secret":false}`
	req4 := httptest.NewRequest(http.MethodPost, "/api/v1/variables", strings.NewReader(body4))
	req4.Header.Set("Content-Type", "application/json")
	w4 := httptest.NewRecorder()
	r.ServeHTTP(w4, req4)
	require.Equal(t, http.StatusOK, w4.Code, w4.Body.String())

	// 非法 scope 应 400
	body5 := `{"scope":"bogus","target_id":0,"key":"k","value":"v"}`
	req5 := httptest.NewRequest(http.MethodPost, "/api/v1/variables", strings.NewReader(body5))
	req5.Header.Set("Content-Type", "application/json")
	w5 := httptest.NewRecorder()
	r.ServeHTTP(w5, req5)
	require.Equal(t, http.StatusBadRequest, w5.Code, w5.Body.String())
}
