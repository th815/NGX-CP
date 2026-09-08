package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/logstore"
)

func newTestLogsServer(store logstore.Storage) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewLogsHandler(store)
	r.POST("/api/v1/logs/search", h.Search)
	r.GET("/api/v1/logs/trace/:request_id", h.Trace)
	return r
}

func TestLogsHandler_Search(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	store := logstore.NewMemStorage()
	rows := []logstore.Entry{
		{TS: now.Add(-10 * time.Minute), Node: "web1", Status: 200, URI: "/api/a", RID: "r1", RemoteAddr: "10.0.0.1", RequestRT: 0.1},
		{TS: now.Add(-5 * time.Minute), Node: "web1", Status: 500, URI: "/api/b", RID: "r2", RemoteAddr: "10.0.0.2", RequestRT: 1.5},
	}
	if err := store.Ingest(context.Background(), rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	srv := newTestLogsServer(store)

	// 按 status=500 检索。
	body, _ := json.Marshal(map[string]any{"status": []uint16{500}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/logs/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Items []logstore.Entry `json:"items"`
			Total int64            `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 0 {
		t.Fatalf("code = %d", resp.Code)
	}
	if resp.Data.Total != 1 {
		t.Fatalf("total = %d, want 1", resp.Data.Total)
	}
	if len(resp.Data.Items) != 1 || resp.Data.Items[0].Status != 500 {
		t.Fatalf("unexpected items: %+v", resp.Data.Items)
	}

	// 空结果（rid 不存在）。
	body2, _ := json.Marshal(map[string]any{"rid": "nope"})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/logs/search", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	var resp2 struct {
		Data struct {
			Total int64 `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v body=%s", err, w2.Body.String())
	}
	if resp2.Data.Total != 0 {
		t.Fatalf("total = %d, want 0", resp2.Data.Total)
	}
}

func TestLogsHandler_Search_BadBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	srv := newTestLogsServer(logstore.NewMemStorage())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/logs/search", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestLogsHandler_Trace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	store := logstore.NewMemStorage()
	rows := []logstore.Entry{
		// 同一 rid 跨两节点：web1 首跳（更早），web2 次跳（慢在后端）。
		{TS: now.Add(-2 * time.Second), Node: "web1", Status: 200, URI: "/x", RID: "abc", RemoteAddr: "10.0.0.1", UpstreamAddr: "10.0.0.9:8080", UpstreamRT: 0.05, RequestRT: 0.06},
		{TS: now.Add(-1 * time.Second), Node: "web2", Status: 200, URI: "/x", RID: "abc", RemoteAddr: "10.0.0.9", UpstreamAddr: "10.0.0.10:9090", UpstreamRT: 0.50, RequestRT: 0.55},
		// 不同 rid 不应混入。
		{TS: now, Node: "web1", Status: 200, URI: "/y", RID: "other", RemoteAddr: "10.0.0.2"},
	}
	if err := store.Ingest(context.Background(), rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	srv := newTestLogsServer(store)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/trace/abc", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			RID        string           `json:"rid"`
			Spans      []logstore.Entry `json:"spans"`
			Nodes      []string         `json:"nodes"`
			FirstHop   string           `json:"first_hop"`
			Bottleneck string           `json:"bottleneck"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 0 {
		t.Fatalf("code = %d", resp.Code)
	}
	if resp.Data.RID != "abc" {
		t.Fatalf("rid = %q, want abc", resp.Data.RID)
	}
	if len(resp.Data.Spans) != 2 {
		t.Fatalf("spans = %d, want 2 (different rid must be excluded)", len(resp.Data.Spans))
	}
	// 时间升序：首条应为首跳 web1，末条为 web2。
	if resp.Data.Spans[0].Node != "web1" || resp.Data.Spans[1].Node != "web2" {
		t.Fatalf("spans not ordered by ts asc: %+v", resp.Data.Spans)
	}
	if len(resp.Data.Nodes) != 2 {
		t.Fatalf("nodes = %v, want 2", resp.Data.Nodes)
	}
	if resp.Data.FirstHop != "web1" {
		t.Fatalf("first_hop = %q, want web1", resp.Data.FirstHop)
	}
	if resp.Data.Bottleneck != "web2" {
		t.Fatalf("bottleneck = %q, want web2 (max upstream_rt)", resp.Data.Bottleneck)
	}

	// 不存在的 rid → 空 spans。
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/logs/trace/missing", nil)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	var resp2 struct {
		Data struct {
			Spans []logstore.Entry `json:"spans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v body=%s", err, w2.Body.String())
	}
	if len(resp2.Data.Spans) != 0 {
		t.Fatalf("missing rid spans = %d, want 0", len(resp2.Data.Spans))
	}
}
