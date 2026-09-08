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
	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/ent/node"
	configstore "github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/repo"
	"github.com/th/ngxcp/internal/security"
)

// newTestSecurityServer 构造仅含安全事件路由的测试引擎（不挂 auth 中间件，
// 仅验证 handler 自身逻辑；auth 由 router 中间件负责）。
func newTestSecurityServer(store security.EventStore) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewSecurityHandler(store, nil)
	r.GET("/api/v1/security/events", h.List)
	r.GET("/api/v1/security/events/:id", h.Get)
	r.POST("/api/v1/security/events/:id/handle", h.Handle)
	return r
}

// seedEvents 向内存存储灌入若干事件，返回。
func seedEvents(t *testing.T, store *security.MemEventStore) {
	t.Helper()
	ctx := context.Background()
	cases := []security.Event{
		{RuleID: "r-sql-injection", RuleName: "SQL 注入特征", Level: "CRITICAL", Node: "web1", Sample: "GET /a?x=' OR '", Handled: true, Action: "blocked"},
		{RuleID: "r-cc-flood", RuleName: "CC 洪水", Level: "CRITICAL", Node: "web2", Sample: "GET /", Handled: false, Action: "pending"},
		{RuleID: "r-odd-ua", RuleName: "非常规 UA", Level: "INFO", Node: "web1", Sample: "GET /x", Handled: false, Action: "pending"},
	}
	for _, e := range cases {
		if _, err := store.Create(ctx, e); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func TestSecurityHandler_List(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := security.NewMemEventStore()
	seedEvents(t, store)
	srv := newTestSecurityServer(store)

	// level=CRITICAL → 2 条（1 handled + 1 unhandled）。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/security/events?level=CRITICAL", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Items []security.Event `json:"items"`
			Total int              `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 0 {
		t.Fatalf("code = %d", resp.Code)
	}
	if resp.Data.Total != 2 {
		t.Fatalf("total = %d, want 2", resp.Data.Total)
	}

	// level=CRITICAL & handled=false → 仅 1 条（web2 的 CC 洪水）。
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/security/events?level=CRITICAL&handled=false", nil)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	var resp2 struct {
		Data struct {
			Items []security.Event `json:"items"`
			Total int              `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v body=%s", err, w2.Body.String())
	}
	if resp2.Data.Total != 1 {
		t.Fatalf("total = %d, want 1", resp2.Data.Total)
	}
	if len(resp2.Data.Items) != 1 || resp2.Data.Items[0].RuleID != "r-cc-flood" {
		t.Fatalf("items = %+v, want only r-cc-flood", resp2.Data.Items)
	}
}

func TestSecurityHandler_GetNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := security.NewMemEventStore()
	srv := newTestSecurityServer(store)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/security/events/999", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (event missing), body=%s", w.Code, w.Body.String())
	}
}

func TestSecurityHandler_HandleFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := security.NewMemEventStore()
	seedEvents(t, store)
	srv := newTestSecurityServer(store)
	// 取未处置的 r-cc-flood 事件 id（seed 第 2 条 → id=2）。
	const id = 2

	// 首次处置成功 → 200。
	body, _ := json.Marshal(map[string]string{"action": "blocked"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/security/events/2/handle", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first handle status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	// 重复处置 → 409 冲突（防重复动作）。
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/security/events/2/handle", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second handle status = %d, want 409 (already handled)", w2.Code)
	}

	// 非法动作 → 400。
	bodyBad, _ := json.Marshal(map[string]string{"action": "banhammer"})
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/security/events/3/handle", bytes.NewReader(bodyBad))
	req3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("illegal action status = %d, want 400, body=%s", w3.Code, w3.Body.String())
	}
}

// TestSecurityHandler_SchedulerDedup 验证同一持续攻击只建一条 pending 事件
// （而非每个周期刷一堆重复事件）。FakeBackend 恒定超限 → 每条规则每周期都命中；
// 调度去重须保证每个 rule 只落 1 条未处置事件。
func TestSecurityHandler_SchedulerDedup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := security.NewMemEventStore()
	engine := security.NewEngine(&security.FakeBackend{Count: 999}) // 恒定超过所有阈值
	sched := security.NewScheduler(engine, store, nil, security.DefaultRules(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	go sched.Start(ctx, 30*time.Millisecond) // 约 5 个周期
	time.Sleep(200 * time.Millisecond)
	cancel()

	want := len(security.DefaultRules()) // 每条规则恰好 1 条 pending
	items, total, err := store.List(ctx, security.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != want {
		t.Fatalf("total events = %d, want %d (each rule exactly one pending; dedup failed across ticks)", total, want)
	}
	// 每个 rule 仅出现一次。
	seen := make(map[string]int)
	for _, e := range items {
		seen[e.RuleID]++
	}
	for rid, c := range seen {
		if c != 1 {
			t.Errorf("rule %s has %d pending events, want 1 (dedup broken)", rid, c)
		}
	}
}

// TestSecurityHandler_BlockEndpoints 验证封禁 HTTP 接线：
// 从事件一键封禁 / 直接封禁 IP / 解封 IP 三个端点都走通并返回 security_block 变更单。
func TestSecurityHandler_BlockEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	client, err := repo.Open("sqlite", "file:handler_block?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(ctx))

	// 一个 online 的 Nginx RS 节点
	_, err = client.Node.Create().
		SetName("rs-nginx-01").SetAddress("10.0.1.11").
		SetRole(node.RoleRealServer).SetStatus(node.StatusOnline).Save(ctx)
	require.NoError(t, err)

	// 事件存储灌入一条带 IP 样本的事件
	evStore := security.NewMemEventStore()
	_, err = evStore.Create(ctx, security.Event{
		RuleID: "r-cc-flood", RuleName: "CC 洪水", Level: "CRITICAL",
		Node: "rs-nginx-01", Sample: "client 192.0.2.33 hit 700 req/min", Handled: false, Action: "pending",
	})
	require.NoError(t, err)

	bs := security.NewBlockService(client, deploy.New(client), configstore.New(client))
	h := NewSecurityHandler(evStore, bs)
	r := gin.New()
	r.POST("/api/v1/security/events/:id/block", h.BlockEvent)
	r.POST("/api/v1/security/blocklist", h.Block)
	r.DELETE("/api/v1/security/blocklist/:ip", h.Unblock)

	// 1) 从事件一键封禁：提取样本 IP 192.0.2.33
	req := httptest.NewRequest(http.MethodPost, "/api/v1/security/events/1/block",
		bytes.NewReader([]byte(`{"operator":"admin"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("block-event status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var evResp struct {
		Data struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &evResp))
	if evResp.Data.Type != "security_block" {
		t.Fatalf("block-event order type = %q, want security_block", evResp.Data.Type)
	}

	// 2) 直接封禁另一个 IP
	body, _ := json.Marshal(map[string]string{"ip": "203.0.113.77", "reason": "手动", "operator": "admin"})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/security/blocklist", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("block status = %d, want 200, body=%s", w2.Code, w2.Body.String())
	}

	// 3) 解封该 IP
	req3 := httptest.NewRequest(http.MethodDelete, "/api/v1/security/blocklist/203.0.113.77", nil)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("unblock status = %d, want 200, body=%s", w3.Code, w3.Body.String())
	}
}
