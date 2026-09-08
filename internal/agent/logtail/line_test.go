package logtail

import (
	"strings"
	"testing"
	"time"
)

// combinedPure 是 nginx 默认 combined 格式的一行（无 upstream 尾巴）。
const combinedPure = `192.168.1.1 - - [10/Sep/2026:13:00:00 +0800] "GET /api/v1/x HTTP/1.1" 200 1234 "https://ref.example/" "Mozilla/5.0 (X11)"`

// combinedUpstream 是常见运维变体：末尾带 request_time upstream_addr upstream_status upstream_rt request_id。
const combinedUpstream = `10.0.0.9 - - [10/Sep/2026:13:00:01 +0800] "POST /pay HTTP/1.1" 502 57 "-" "curl/8.0" 1.234 10.0.0.2:8080 502 0.045, 0.012 abc-rid-123`

// combinedReqOnly 是只额外附加了 $request_time 的格式。
const combinedReqOnly = `10.0.0.9 - - [10/Sep/2026:13:00:02 +0800] "GET /health HTTP/1.1" 200 2 "-" "kube-probe" 0.003`

func TestParseStandardLine_PureCombined(t *testing.T) {
	l := parseStandardLine(combinedPure, "node-1")
	if l.Node != "node-1" {
		t.Errorf("node = %q", l.Node)
	}
	if l.RemoteAddr != "192.168.1.1" {
		t.Errorf("remote = %q", l.RemoteAddr)
	}
	if l.URI != "/api/v1/x" {
		t.Errorf("uri = %q", l.URI)
	}
	if l.Status != 200 {
		t.Errorf("status = %d", l.Status)
	}
	if l.Bytes != 1234 {
		t.Errorf("bytes = %d", l.Bytes)
	}
	if l.UA != "Mozilla/5.0 (X11)" {
		t.Errorf("ua = %q", l.UA)
	}
	// 无尾巴：upstream/rt 应为零值。
	if l.RequestRT != 0 || l.UpstreamAddr != "" || l.UpstreamRT != 0 || l.RID != "" {
		t.Errorf("trailing should be empty: %+v", l)
	}
	// 时间：CLF → RFC3339，且能被控制面 DefaultParseTS 解析。
	if l.TS == "" {
		t.Fatal("TS should be converted from CLF")
	}
	if _, err := time.Parse(time.RFC3339Nano, l.TS); err != nil {
		t.Errorf("TS %q not RFC3339-parseable: %v", l.TS, err)
	}
	if l.Raw != combinedPure {
		t.Errorf("Raw should be preserved")
	}
}

func TestParseStandardLine_UpstreamSuffix(t *testing.T) {
	l := parseStandardLine(combinedUpstream, "node-2")
	if l.Status != 502 {
		t.Errorf("status = %d", l.Status)
	}
	if l.RequestRT != 1.234 {
		t.Errorf("request_rt = %v", l.RequestRT)
	}
	if l.UpstreamAddr != "10.0.0.2:8080" {
		t.Errorf("upstream_addr = %q", l.UpstreamAddr)
	}
	if l.UpstreamStatus != "502" {
		t.Errorf("upstream_status = %q", l.UpstreamStatus)
	}
	// $upstream_response_time 是 "0.045, 0.012" → 取首个 0.045
	if l.UpstreamRT != 0.045 {
		t.Errorf("upstream_rt = %v (want 0.045)", l.UpstreamRT)
	}
	if l.RID != "abc-rid-123" {
		t.Errorf("rid = %q", l.RID)
	}
}

func TestParseStandardLine_ReqOnly(t *testing.T) {
	l := parseStandardLine(combinedReqOnly, "n")
	if l.Status != 200 {
		t.Errorf("status = %d", l.Status)
	}
	if l.RequestRT != 0.003 {
		t.Errorf("request_rt = %v (want 0.003)", l.RequestRT)
	}
	if l.UpstreamAddr != "" || l.RID != "" {
		t.Errorf("no upstream/rid expected: %+v", l)
	}
}

func TestParseStandardLine_ErrorClass(t *testing.T) {
	if !parseStandardLine(combinedUpstream, "n").IsErrorClass() {
		t.Error("502 should be error class")
	}
	if parseStandardLine(combinedPure, "n").IsErrorClass() {
		t.Error("200 should not be error class")
	}
}

func TestParseStandardLine_NonMatching(t *testing.T) {
	// error_log 行或脏数据：匹配不上 combined → 保留 Raw、Status 0。
	bad := parseStandardLine("2026/09/10 13:00:00 [error] 1234#0: *56 connect() failed", "n")
	if bad.Status != 0 {
		t.Errorf("status = %d, want 0", bad.Status)
	}
	if bad.Raw == "" {
		t.Error("Raw must be preserved")
	}
	// 采样下应保留（无法解析不丢）。
	if !sampleKeep(bad, 0.001, func() float64 { return 1.0 }) {
		t.Error("unparseable must be kept under sampling")
	}
}

func TestParseLine_AutoDetect(t *testing.T) {
	// JSON 行仍按 JSON 解析（自动探测）。
	jsonLine := `{"time":"2026-09-08T00:00:00+00:00","rid":"r1","remote_addr":"10.0.0.1","uri":"/j","status":200,"bytes":10,"ua":"curl"}`
	jl := ParseLine(jsonLine, "node-x")
	if jl.Status != 200 || jl.RID != "r1" || jl.URI != "/j" {
		t.Errorf("JSON auto-detect failed: %+v", jl)
	}
	// 文本行按标准解析（自动探测）。
	tl := ParseLine(combinedPure, "node-x")
	if tl.Status != 200 || tl.RemoteAddr != "192.168.1.1" || tl.URI != "/api/v1/x" {
		t.Errorf("text auto-detect failed: %+v", tl)
	}
}

func TestTailer_parseLine_RespectsFormat(t *testing.T) {
	// Format="json" 强制 JSON 解析，即使文本行传入也不按文本解。
	tj := &Tailer{Format: "json", Node: "n"}
	jl := tj.parseLine(combinedPure)
	if jl.Status != 0 {
		t.Errorf("Format=json should NOT parse text as combined, got status %d", jl.Status)
	}
	// Format="combined" 强制文本解析。
	tc := &Tailer{Format: "combined", Node: "n"}
	cl := tc.parseLine(combinedPure)
	if cl.Status != 200 {
		t.Errorf("Format=combined should parse text, got status %d", cl.Status)
	}
	// Format="" 走自动探测（文本）。
	ta := &Tailer{Format: "", Node: "n"}
	al := ta.parseLine(combinedPure)
	if al.Status != 200 {
		t.Errorf("Format=\"\" should auto-detect text, got status %d", al.Status)
	}
	_ = strings.TrimSpace // 确保 strings 包被使用（解析器内部依赖）
}
