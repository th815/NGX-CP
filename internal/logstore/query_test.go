package logstore

import (
	"context"
	"strings"
	"testing"
	"time"
)

func mkEntry(ts time.Time, node string, status uint16, uri, rid, ip string, rt float32) Entry {
	return Entry{
		TS:         ts,
		Node:       node,
		Status:     status,
		URI:        uri,
		RID:        rid,
		RemoteAddr: ip,
		RequestRT:  rt,
	}
}

func TestEscapeLike(t *testing.T) {
	got := escapeLike("a%b_c")
	if got != `a\%b\_c` {
		t.Fatalf("escapeLike = %q, want a\\%%b\\_c", got)
	}
}

// TestBuildWhere_Parameterized：验证所有用户输入都走 ? 占位参数，字符串不被拼接。
func TestBuildWhere_Parameterized(t *testing.T) {
	p := QueryParams{
		TimeFrom: time.Now().Add(-time.Hour),
		TimeTo:   time.Now(),
		Nodes:    []string{"n1", "n2"},
		Status:   []uint16{500, 502},
		URI:      "admin'/drop",
		IP:       "1.2.3.4",
		RID:      "r1",
		RTMin:    0.5,
	}
	where, args := buildWhere(p)
	// 必须有占位符，且 URI 原样出现在 args 中（未被拼接进 SQL 字符串）。
	if want := 0; countPlaceholders(where) < want {
		t.Fatalf("expected >=%d placeholders, got sql=%q", want, where)
	}
	foundURI := false
	for _, a := range args {
		if s, ok := a.(string); ok && strings.Contains(s, "admin'/drop") {
			foundURI = true
		}
	}
	if !foundURI {
		t.Fatalf("URI not passed as bound parameter: args=%v", args)
	}
	if has := containsRaw(where, "drop"); has {
		t.Fatalf("raw user input leaked into SQL: %q", where)
	}
}

func countPlaceholders(s string) int {
	n := 0
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '?' {
			n++
		}
	}
	return n
}

func containsRaw(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestMemStorage_Query_Filters(t *testing.T) {
	now := time.Now()
	m := NewMemStorage()
	base := now.Add(-2 * time.Hour)
	rows := []Entry{
		mkEntry(base.Add(10*time.Minute), "web1", 200, "/api/a", "r1", "10.0.0.1", 0.1),
		mkEntry(base.Add(20*time.Minute), "web1", 500, "/api/b", "r2", "10.0.0.2", 1.2),
		mkEntry(base.Add(30*time.Minute), "web2", 500, "/admin/x", "r3", "10.0.0.3", 2.5),
		mkEntry(base.Add(40*time.Minute), "web2", 404, "/api/c", "r4", "10.0.0.1", 0.3),
	}
	if err := m.Ingest(context.Background(), rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	cases := []struct {
		name string
		p    QueryParams
		want int
	}{
		{"status 500", QueryParams{Status: []uint16{500}}, 2},
		{"node web2", QueryParams{Nodes: []string{"web2"}}, 2},
		{"uri substring admin", QueryParams{URI: "admin"}, 1},
		{"uri regex ^/api", QueryParams{URI: "^/api", Regex: true}, 3},
		{"rid r3", QueryParams{RID: "r3"}, 1},
		{"ip 10.0.0.1", QueryParams{IP: "10.0.0.1"}, 2},
		{"rt>=1", QueryParams{RTMin: 1.0}, 2},
		{"combined status+node", QueryParams{Nodes: []string{"web1"}, Status: []uint16{200}}, 1},
		{"no match", QueryParams{RID: "nope"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := m.Query(context.Background(), c.p)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if int(res.Total) != c.want {
				t.Fatalf("total = %d, want %d (params=%+v)", res.Total, c.want, c.p)
			}
			if len(res.Items) != c.want {
				t.Fatalf("items = %d, want %d", len(res.Items), c.want)
			}
		})
	}
}

func TestMemStorage_Query_PaginationAndOrder(t *testing.T) {
	now := time.Now()
	m := NewMemStorage()
	rows := make([]Entry, 0, 5)
	for i := 0; i < 5; i++ {
		// 落在默认 24h 窗内（过去 1~5 分钟）。
		rows = append(rows, mkEntry(now.Add(-time.Duration(i+1)*time.Minute), "n", 200, "/p", "r", "1.1.1.1", 0))
	}
	if err := m.Ingest(context.Background(), rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// 默认时间窗近 24h 应包含全部 5 条。
	res, err := m.Query(context.Background(), QueryParams{Page: 1, Size: 2})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Total != 5 {
		t.Fatalf("total = %d, want 5", res.Total)
	}
	if len(res.Items) != 2 {
		t.Fatalf("page size = %d, want 2", len(res.Items))
	}
	// 校验 ts 降序：第 1 页首条应是第 4 分钟（最新）。
	if !res.Items[0].TS.After(res.Items[1].TS) {
		t.Fatalf("items not ordered desc by ts: %v vs %v", res.Items[0].TS, res.Items[1].TS)
	}
	// 第 2 页取后续 2 条。
	res2, _ := m.Query(context.Background(), QueryParams{Page: 2, Size: 2})
	if len(res2.Items) != 2 {
		t.Fatalf("page2 size = %d, want 2", len(res2.Items))
	}
	// 第 3 页只剩 1 条。
	res3, _ := m.Query(context.Background(), QueryParams{Page: 3, Size: 2})
	if len(res3.Items) != 1 {
		t.Fatalf("page3 size = %d, want 1", len(res3.Items))
	}
}

func TestMemStorage_Query_DefaultTimeWindow(t *testing.T) {
	now := time.Now()
	m := NewMemStorage()
	// 一条 48h 前的旧日志，应被默认 24h 窗排除。
	old := mkEntry(now.Add(-48*time.Hour), "n", 200, "/old", "r", "1.1.1.1", 0)
	// 一条 1h 前，应在窗内。
	recent := mkEntry(now.Add(-1*time.Hour), "n", 200, "/new", "r", "1.1.1.1", 0)
	if err := m.Ingest(context.Background(), []Entry{old, recent}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	res, err := m.Query(context.Background(), QueryParams{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("total = %d, want 1 (default 24h window should exclude 48h-old)", res.Total)
	}
}
