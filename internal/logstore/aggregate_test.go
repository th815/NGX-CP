package logstore

import (
	"testing"
	"time"
)

// mkAggEntry 构造一条日志 Entry（ts 用 now 偏移，便于落在时间窗内）。
func mkAggEntry(ts time.Time, node, uri, ip, ua string, status uint16, reqRT float32) Entry {
	return Entry{
		TS:         ts,
		Node:       node,
		URI:        uri,
		RemoteAddr: ip,
		UA:         ua,
		Status:     status,
		RequestRT:  reqRT,
	}
}

func TestComputeAgg_TopURI(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		mkAggEntry(now, "rs1", "/a", "1.1.1.1", "ua1", 200, 0.1),
		mkAggEntry(now, "rs1", "/a", "1.1.1.2", "ua2", 500, 0.2), // /a 的一条 5xx
		mkAggEntry(now, "rs1", "/b", "1.1.1.3", "ua3", 200, 0.3),
		mkAggEntry(now, "rs1", "/b", "1.1.1.4", "ua4", 200, 0.4),
		mkAggEntry(now, "rs1", "/b", "1.1.1.5", "ua5", 200, 0.5),
	}
	res := computeAgg(entries, AggParams{Metric: AggTopURI, TopN: 10})
	if len(res.Rows) != 2 {
		t.Fatalf("top_uri rows = %d, want 2", len(res.Rows))
	}
	// 第一行应为 /b（3 条），第二行为 /a（2 条）。
	if res.Rows[0].Key != "/b" || res.Rows[0].Count != 3 {
		t.Errorf("row0 = %+v, want /b count 3", res.Rows[0])
	}
	// /a 含 1 条错误（500）。
	if res.Rows[1].Key != "/a" || res.Rows[1].Count != 2 || res.Rows[1].Err != 1 {
		t.Errorf("row1 = %+v, want /a count 2 err 1", res.Rows[1])
	}
}

func TestComputeAgg_TopURI_Cap(t *testing.T) {
	now := time.Now()
	entries := make([]Entry, 0, 12)
	for i := 0; i < 12; i++ {
		entries = append(entries, mkAggEntry(now, "rs1", string(rune('a'+i)), "1.1.1.1", "ua", 200, 0.1))
	}
	res := computeAgg(entries, AggParams{Metric: AggTopURI, TopN: 5})
	if len(res.Rows) != 5 {
		t.Fatalf("top_uri rows = %d, want 5 (TopN cap)", len(res.Rows))
	}
}

func TestComputeAgg_StatusDist(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		mkAggEntry(now, "rs1", "/a", "1.1.1.1", "ua", 200, 0.1),
		mkAggEntry(now, "rs1", "/a", "1.1.1.2", "ua", 200, 0.2),
		mkAggEntry(now, "rs1", "/a", "1.1.1.3", "ua", 404, 0.3),
		mkAggEntry(now, "rs1", "/a", "1.1.1.4", "ua", 500, 0.4),
	}
	res := computeAgg(entries, AggParams{Metric: AggStatusDist})
	if len(res.Rows) != 3 {
		t.Fatalf("status_dist rows = %d, want 3", len(res.Rows))
	}
	// 200 应排第一，count=2。
	if res.Rows[0].Key != "200" || res.Rows[0].Count != 2 {
		t.Errorf("row0 = %+v, want 200 count 2", res.Rows[0])
	}
}

func TestComputeAgg_RTPercentile(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		mkAggEntry(now, "rs1", "/a", "1.1.1.1", "ua", 200, 0.1),
		mkAggEntry(now, "rs1", "/a", "1.1.1.2", "ua", 200, 0.2),
		mkAggEntry(now, "rs1", "/a", "1.1.1.3", "ua", 200, 0.3),
		mkAggEntry(now, "rs1", "/a", "1.1.1.4", "ua", 200, 0.4),
		mkAggEntry(now, "rs1", "/a", "1.1.1.5", "ua", 200, 1.0),
	}
	res := computeAgg(entries, AggParams{Metric: AggRTPercentile})
	if len(res.Rows) != 1 {
		t.Fatalf("rt_percentile rows = %d, want 1", len(res.Rows))
	}
	r := res.Rows[0]
	// 值集 [0.1,0.2,0.3,0.4,1.0]，n=5：
	//   P50 = idx 2.0 → 0.3；P95 = idx 3.8 → 0.4+0.8*(1.0-0.4)=0.88；P99 = idx 3.96 → 0.976。
	if r.P50 < 0.29 || r.P50 > 0.31 {
		t.Errorf("P50 = %v, want ~0.3", r.P50)
	}
	if r.P95 < 0.85 || r.P95 > 0.91 {
		t.Errorf("P95 = %v, want ~0.88", r.P95)
	}
	if r.P99 < 0.95 || r.P99 > 0.99 {
		t.Errorf("P99 = %v, want ~0.976", r.P99)
	}
}

func TestComputeAgg_Empty(t *testing.T) {
	res := computeAgg(nil, AggParams{Metric: AggTopURI})
	if res == nil || len(res.Rows) != 0 || res.Total != 0 {
		t.Fatalf("empty agg = %+v, want empty", res)
	}
	res2 := computeAgg(nil, AggParams{Metric: AggRTPercentile})
	if res2.Rows[0].P50 != 0 {
		t.Fatalf("empty rt percentile P50 = %v, want 0", res2.Rows[0].P50)
	}
}

func TestParseWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"":   24 * time.Hour,
		"1h": time.Hour,
		"24h": 24 * time.Hour,
		"7d": 7 * 24 * time.Hour,
		"bad": 24 * time.Hour,
		"0":  24 * time.Hour,
	}
	for in, want := range cases {
		if got := parseWindow(in); got != want {
			t.Errorf("parseWindow(%q) = %v, want %v", in, got, want)
		}
	}
}
