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
