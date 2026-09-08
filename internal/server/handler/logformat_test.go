// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T060 端点行为测试。用 fake 编排器隔离 DB / 节点，只验证 handler 契约：
// 契约自述可读、预览逐节点列举失败原因（而非首台失败即整体 500）、下发透传编排结果。
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/domain/logfmt"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// fakeLogFmt 按 nodeID 决定成败：偶数节点成功，奇数节点返回前置条件错误。
type fakeLogFmt struct {
	applyIn  logfmt.ApplyInput
	applyErr error
}

func (f *fakeLogFmt) Plan(_ context.Context, nodeID int, opts logfmt.SnippetOptions) (*logfmt.NodePlan, error) {
	if nodeID%2 == 1 {
		return nil, apperr.New(apperr.CodePrecondition, "未在 http{} 内发现通配 include")
	}
	opts.Normalize()
	return &logfmt.NodePlan{
		NodeID:      nodeID,
		NginxVer:    "1.30.0",
		SnippetPath: "/etc/nginx/conf.d/" + logfmt.SnippetFileName,
		LogPath:     opts.LogPath,
		FormatName:  opts.FormatName,
		IncludeDir:  "/etc/nginx/conf.d",
		Content:     logfmt.RenderSnippet(opts),
		Fields:      logfmt.FieldKeys(),
		Warnings:    []string{"双写告警"},
	}, nil
}

func (f *fakeLogFmt) Apply(_ context.Context, in logfmt.ApplyInput) (*logfmt.ApplyResult, error) {
	f.applyIn = in
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return &logfmt.ApplyResult{
		ChangeOrderID: 77,
		Status:        "draft",
		FormatName:    logfmt.DefaultFormatName,
		Fields:        logfmt.FieldKeys(),
		Nodes: []*logfmt.NodeApply{
			{NodeID: 2, RevisionID: 9, SHA256: "abc"},
		},
	}, nil
}

func newLogFormatTestServer(svc LogFormatOrchestrator) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewLogFormatHandler(svc)
	r.GET("/api/v1/logs/format", h.Contract)
	r.POST("/api/v1/logs/format/preview", h.Preview)
	r.POST("/api/v1/logs/format/apply", h.Apply)
	return r
}

// 请求助手 doJSON 复用 deploy_test.go 中的实现（同包共享，避免两套语义分叉）。

func TestLogFormatHandler_Contract(t *testing.T) {
	srv := newLogFormatTestServer(&fakeLogFmt{})
	w := doJSON(t, srv, http.MethodGet, "/api/v1/logs/format", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			FormatName string   `json:"format_name"`
			Fields     []string `json:"fields"`
			MinVer     string   `json:"min_nginx_version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.FormatName != logfmt.DefaultFormatName {
		t.Errorf("format_name = %q", resp.Data.FormatName)
	}
	if len(resp.Data.Fields) != len(logfmt.FieldKeys()) {
		t.Errorf("fields 数量 = %d, want %d", len(resp.Data.Fields), len(logfmt.FieldKeys()))
	}
	// rid 是 TraceID 的根，缺了 T064 全链路直接失效——单独断言。
	found := false
	for _, f := range resp.Data.Fields {
		if f == "rid" {
			found = true
		}
	}
	if !found {
		t.Error("契约字段缺少 rid")
	}
	if resp.Data.MinVer != logfmt.MinNginxVersion {
		t.Errorf("min_nginx_version = %q", resp.Data.MinVer)
	}
}

// 预览必须把每台的失败原因都返回，且 appliable=false，
// 否则用户只能看到第一台报错、需要逐台试错。
func TestLogFormatHandler_PreviewListsBlockedPerNode(t *testing.T) {
	srv := newLogFormatTestServer(&fakeLogFmt{})
	w := doJSON(t, srv, http.MethodPost, "/api/v1/logs/format/preview", map[string]any{
		"node_ids": []int{1, 2, 3},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Nodes []struct {
				NodeID  int    `json:"node_id"`
				LogPath string `json:"log_path"`
				Content string `json:"content"`
			} `json:"nodes"`
			Blocked []struct {
				NodeID  int    `json:"node_id"`
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"blocked"`
			Appliable bool `json:"appliable"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(resp.Data.Nodes) != 1 || resp.Data.Nodes[0].NodeID != 2 {
		t.Fatalf("nodes = %+v", resp.Data.Nodes)
	}
	if len(resp.Data.Blocked) != 2 {
		t.Fatalf("blocked = %+v, want 2 条（节点 1、3）", resp.Data.Blocked)
	}
	if resp.Data.Appliable {
		t.Error("存在阻断节点时 appliable 应为 false")
	}
	// 预览必须返回将写入的完整内容（「所见即所发」）。
	if !bytes.Contains([]byte(resp.Data.Nodes[0].Content), []byte("escape=json")) {
		t.Errorf("预览内容未含 escape=json: %q", resp.Data.Nodes[0].Content)
	}
}

func TestLogFormatHandler_PreviewRejectsEmptyNodes(t *testing.T) {
	srv := newLogFormatTestServer(&fakeLogFmt{})
	w := doJSON(t, srv, http.MethodPost, "/api/v1/logs/format/preview", map[string]any{})
	if w.Code == http.StatusOK {
		t.Fatalf("空 node_ids 应被拒绝，got %d body=%s", w.Code, w.Body.String())
	}
}

func TestLogFormatHandler_ApplyPassesThrough(t *testing.T) {
	fake := &fakeLogFmt{}
	srv := newLogFormatTestServer(fake)
	w := doJSON(t, srv, http.MethodPost, "/api/v1/logs/format/apply", map[string]any{
		"node_ids":    []int{2, 4},
		"format_name": "my_json",
		"created_by":  "tianhao",
		"comment":     "统一日志格式",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data logfmt.ApplyResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.ChangeOrderID != 77 || resp.Data.Status != "draft" {
		t.Errorf("apply 结果 = %+v，应为 draft 变更单 77", resp.Data)
	}
	if len(fake.applyIn.NodeIDs) != 2 || fake.applyIn.Options.FormatName != "my_json" {
		t.Errorf("请求未正确透传: %+v", fake.applyIn)
	}
	if fake.applyIn.CreatedBy != "tianhao" {
		t.Errorf("created_by 未透传: %q", fake.applyIn.CreatedBy)
	}
}

func TestLogFormatHandler_ApplyPropagatesError(t *testing.T) {
	fake := &fakeLogFmt{applyErr: apperr.New(apperr.CodePrecondition, "nginx 1.10.3 不支持 escape=json")}
	srv := newLogFormatTestServer(fake)
	w := doJSON(t, srv, http.MethodPost, "/api/v1/logs/format/apply", map[string]any{"node_ids": []int{2}})
	if w.Code == http.StatusOK {
		t.Fatalf("编排失败应返回非 200，got %d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("escape=json")) {
		t.Errorf("错误信息未透传: %s", w.Body.String())
	}
}
