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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/domain/config"
)

// fakeDrift 实现 config.DriftChecker，返回预设报告，验证 HTTP 层映射。
type fakeDrift struct {
	reports map[int]*config.DriftReport
}

func newFakeDrift() *fakeDrift { return &fakeDrift{reports: map[int]*config.DriftReport{}} }

func (f *fakeDrift) Detect(_ context.Context, nodeID int, _ []config.ReportedConfigFile) (*config.DriftReport, error) {
	return &config.DriftReport{NodeID: nodeID, CheckedAt: time.Now()}, nil
}
func (f *fakeDrift) RecordActual(_ context.Context, nodeID int, _ []config.ReportedConfigFile) (*config.DriftReport, error) {
	r := &config.DriftReport{
		NodeID:    nodeID,
		CheckedAt: time.Now(),
		Items: []config.DriftItem{{
			Path: "/etc/nginx/conf.d/api.conf", Kind: config.DriftModified,
			ExpectedSHA: "aaa", ActualSHA: "bbb", Severity: "critical", DetectedAt: time.Now(),
		}},
	}
	f.reports[nodeID] = r
	return r, nil
}
func (f *fakeDrift) GetReport(nodeID int) (*config.DriftReport, bool) {
	r, ok := f.reports[nodeID]
	return r, ok
}
func (f *fakeDrift) Reports() []*config.DriftReport {
	out := make([]*config.DriftReport, 0, len(f.reports))
	for _, r := range f.reports {
		out = append(out, r)
	}
	return out
}
func (f *fakeDrift) RunWorker(_ context.Context, _ time.Duration) error { return nil }

func newDriftRouter(fd *fakeDrift) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewDriftHandler(fd)
	r := gin.New()
	r.GET("/api/v1/configs/drift", h.ListDrift)
	r.POST("/api/v1/configs/drift", h.CheckDrift)
	return r
}

func TestDriftHandler_EmptyThenCheck(t *testing.T) {
	fd := newFakeDrift()
	r := newDriftRouter(fd)

	// 1) 未产生报告 → 200 + 空 items。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/configs/drift?node_id=1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
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

	// 2) POST 提交 actual → 触发检测 → 200 + 1 条 critical。
	body := `{"node_id":1,"files":[{"path":"/etc/nginx/conf.d/api.conf","sha":"bbb","content":"changed"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/configs/drift", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w2.Code, w2.Body.String())
	}
	var out2 map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &out2); err != nil {
		t.Fatal(err)
	}
	data2 := out2["data"].(map[string]any)
	if int(data2["count"].(float64)) != 1 {
		t.Fatalf("count 期望 1，实际 %v", data2["count"])
	}
	if int(data2["critical"].(float64)) != 1 {
		t.Fatalf("critical 期望 1，实际 %v", data2["critical"])
	}

	// 3) 再 GET 该节点 → 返回已缓存的报告。
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/configs/drift?node_id=1", nil)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	var out3 map[string]any
	if err := json.Unmarshal(w3.Body.Bytes(), &out3); err != nil {
		t.Fatal(err)
	}
	data := out3["data"].(map[string]any)
	items := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("缓存报告应有 1 条漂移，实际 %d", len(items))
	}
}

func TestDriftHandler_AllReports(t *testing.T) {
	fd := newFakeDrift()
	r := newDriftRouter(fd)
	// 先 POST 一次。
	body := `{"node_id":2,"files":[{"path":"/etc/nginx/nginx.conf","sha":"x","content":"y"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/configs/drift", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(httptest.NewRecorder(), req)

	// 无 node_id → 返回全部报告列表。
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/configs/drift", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req2)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if int(out["total"].(float64)) != 1 {
		t.Fatalf("total 期望 1，实际 %v", out["total"])
	}
	items := out["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("data 列表期望 1 条，实际 %d", len(items))
	}
}
