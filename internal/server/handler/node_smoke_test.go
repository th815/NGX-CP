// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T018-C 端点冒烟测试：用内存 sqlite 装配 node.Service，验证三个 T018 新增端点的端到端行为：
//   - GET /nodes/:id/capability 返回真实能力视图（含 nginx 画像 + 系统信息 + 配置树 + 日志目标）
//   - GET /nodes/:id/config-files 返回配置树文件元数据
//   - GET /nodes/:id/log-targets 返回日志采集目标
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/ent"
	entnode "github.com/th/ngxcp/ent/node"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/repo"
	"github.com/gin-gonic/gin"
)

// setupNode 起内存 sqlite、建表并创建一个 enrolling 节点，返回 client 与节点 ID。
func setupNode(t *testing.T) (*ent.Client, int) {
	t.Helper()
	client, err := repo.Open("sqlite", "file:ngxcp_smoke?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	n, err := client.Node.Create().
		SetName("rs-smoke").
		SetAddress("10.0.0.50").
		SetRole(entnode.RoleRealServer).
		SetStatus(entnode.StatusEnrolling).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	return client, n.ID
}

// ginCtx 构造携带 :id 参数的 gin 测试上下文。
func ginCtx(w *httptest.ResponseRecorder, id int) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/nodes/"+strconv.Itoa(id), nil)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
	return c
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("解析响应体失败: %v\nbody=%s", err, w.Body.String())
	}
	return m
}

// TestNodeHandlerCapabilityAndTargets 验证 T018-C 三个新增端点的端到端行为。
func TestNodeHandlerCapabilityAndTargets(t *testing.T) {
	client, id := setupNode(t)
	defer client.Close()
	svc := node.New(client)
	h := NewNodeHandler(svc, nil)
	ctx := context.Background()

	// 落库：能力基线 + 配置树 + 日志目标。
	if err := svc.SaveCapability(ctx, id, node.CapabilityIn{
		Hostname:     "rs-smoke",
		OS:           "rocky 9.4",
		Kernel:       "5.14.0",
		NginxVersion: "1.30.0",
		NginxPrefix:  "/etc/nginx",
		NginxModules: []string{"http_ssl", "stream"},
		NginxRawArgs: "--prefix=/etc/nginx",
		ConfigHash:   "deadbeef",
		SystemInfo:   &node.SystemInfoView{OS: "rocky 9.4", UlimitNofile: 1024, NTPSynced: true},
	}); err != nil {
		t.Fatalf("SaveCapability: %v", err)
	}
	if err := svc.SaveConfigTree(ctx, id, []*agentv1.ConfigFile{
		{Path: "/etc/nginx/nginx.conf", Sha256: "abc", Size: 100},
		{Path: "/etc/nginx/conf.d/default.conf", Sha256: "def", Size: 50},
	}); err != nil {
		t.Fatalf("SaveConfigTree: %v", err)
	}
	if err := svc.SaveLogTargets(ctx, id, []*agentv1.LogTarget{
		{Path: "/var/log/nginx/access.log", Type: "access", Format: "main", Size: 10, Inode: 7},
		{Path: "", Type: "error", IsOff: true, SkipReason: "off"},
	}); err != nil {
		t.Fatalf("SaveLogTargets: %v", err)
	}

	// capability 端点：返回真实视图（含 nginx 画像 + 系统信息 + 配置树 + 日志目标）。
	w := httptest.NewRecorder()
	h.GetCapability(ginCtx(w, id))
	if w.Code != http.StatusOK {
		t.Fatalf("GetCapability status = %d, want 200", w.Code)
	}
	data := decodeBody(t, w)["data"].(map[string]any)
	nginx, ok := data["nginx"].(map[string]any)
	if !ok || nginx["version"] != "1.30.0" {
		t.Fatalf("capability.nginx.version 缺失或错误: %v", data["nginx"])
	}
	if mods, _ := nginx["modules"].([]any); len(mods) != 2 {
		t.Errorf("capability.nginx.modules = %v, want 2", mods)
	}
	if data["system"] == nil {
		t.Error("capability 未含系统信息")
	}
	if cfgFiles, _ := data["config_files"].([]any); len(cfgFiles) != 2 {
		t.Errorf("capability.config_files 数 = %d, want 2", len(cfgFiles))
	}
	if targets, _ := data["log_targets"].([]any); len(targets) != 2 {
		t.Errorf("capability.log_targets 数 = %d, want 2", len(targets))
	}

	// config-files 端点。
	w2 := httptest.NewRecorder()
	h.ConfigFiles(ginCtx(w2, id))
	if w2.Code != http.StatusOK {
		t.Fatalf("ConfigFiles status = %d, want 200", w2.Code)
	}
	if cf, _ := decodeBody(t, w2)["data"].([]any); len(cf) != 2 {
		t.Errorf("config-files 返回 %d 条, want 2", len(cf))
	}

	// log-targets 端点。
	w3 := httptest.NewRecorder()
	h.LogTargets(ginCtx(w3, id))
	if w3.Code != http.StatusOK {
		t.Fatalf("LogTargets status = %d, want 200", w3.Code)
	}
	if lt, _ := decodeBody(t, w3)["data"].([]any); len(lt) != 2 {
		t.Errorf("log-targets 返回 %d 条, want 2", len(lt))
	}
}
