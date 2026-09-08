package logstore

import (
	"testing"
	"time"

	"github.com/th/ngxcp/internal/agent/logtail"
)

// combinedLine 模拟真实 nginx 默认 combined 文本日志（非 JSON）。
const combinedLine = `192.168.1.1 - - [10/Sep/2026:13:00:00 +0800] "GET /api/v1/x HTTP/1.1" 200 1234 "https://ref/" "Mozilla/5.0"`

// TestFromLogLines_StandardText 锁定关键正确性：标准文本日志经 logtail 解析后，
// CLF 时间（$time_local）被转换为 RFC3339 存入 TS，控制面 FromLogLines 能还原出
// 真实时间（非 1970 epoch）。否则所有文本日志的时间窗查询/聚合都会失效。
func TestFromLogLines_StandardText(t *testing.T) {
	lines := []logtail.LogLine{
		logtail.ParseLine(combinedLine, "node-1"),
	}
	entries := FromLogLines(lines, nil)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.TS.IsZero() {
		t.Fatalf("TS must not be zero (text log time conversion broke): %+v", e)
	}
	// 期望接近 2026-09-10T13:00:00+08:00。
	want := time.Date(2026, 9, 10, 13, 0, 0, 0, time.FixedZone("+0800", 8*3600))
	if e.TS.Sub(want).Abs() > 2*time.Second {
		t.Fatalf("TS = %v, want ~%v", e.TS, want)
	}
	if e.Status != 200 {
		t.Errorf("status = %d, want 200", e.Status)
	}
	if e.URI != "/api/v1/x" {
		t.Errorf("uri = %q, want /api/v1/x", e.URI)
	}
	if e.RemoteAddr != "192.168.1.1" {
		t.Errorf("remote = %q", e.RemoteAddr)
	}
	if e.Node != "node-1" {
		t.Errorf("node = %q", e.Node)
	}
}
