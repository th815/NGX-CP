// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package logtail

import "testing"

// TestParseJSONLine_QuotedNumbers 锁 T060 契约：下发格式里所有值都带引号，
// 采集侧必须照常解析出数值。
func TestParseJSONLine_QuotedNumbers(t *testing.T) {
	raw := `{"time":"2026-09-08T13:20:01+08:00","rid":"9f1c","remote_addr":"1.2.3.4",` +
		`"server":"a.example.com","uri":"/api/v1/x?y=1","status":"200",` +
		`"upstream_addr":"10.0.1.11:8080","upstream_status":"200","upstream_rt":"0.031",` +
		`"request_rt":"0.033","bytes":"1024","ua":"curl/8.6.0"}`
	l := ParseLine(raw, "rs1")
	if l.Status != 200 {
		t.Fatalf("status 应为 200，got %d", l.Status)
	}
	if l.Bytes != 1024 {
		t.Fatalf("bytes 应为 1024，got %d", l.Bytes)
	}
	if l.UpstreamRT != 0.031 {
		t.Fatalf("upstream_rt 应为 0.031，got %v", l.UpstreamRT)
	}
	if l.RequestRT != 0.033 {
		t.Fatalf("request_rt 应为 0.033，got %v", l.RequestRT)
	}
	if l.RID != "9f1c" || l.Node != "rs1" || l.URI != "/api/v1/x?y=1" {
		t.Fatalf("字符串字段解析错：%+v", l)
	}
}

// TestParseJSONLine_EmptyUpstream 是最容易翻车的真实场景：静态文件 / redirect 请求
// 没有 upstream，三个 upstream 字段是空串。必须解析成功且其余字段完好。
func TestParseJSONLine_EmptyUpstream(t *testing.T) {
	raw := `{"time":"2026-09-08T13:20:02+08:00","rid":"abc","remote_addr":"1.2.3.4",` +
		`"server":"a.example.com","uri":"/favicon.ico","status":"304",` +
		`"upstream_addr":"","upstream_status":"","upstream_rt":"",` +
		`"request_rt":"0.000","bytes":"0","ua":"Mozilla/5.0"}`
	l := ParseLine(raw, "rs1")
	if l.Status != 304 {
		t.Fatalf("空 upstream 不应影响 status，got %d（整行解析失败的典型症状）", l.Status)
	}
	if l.UpstreamRT != 0 || l.UpstreamAddr != "" {
		t.Fatalf("空 upstream 应退化为零值，got rt=%v addr=%q", l.UpstreamRT, l.UpstreamAddr)
	}
	if l.URI != "/favicon.ico" {
		t.Fatalf("uri 丢失：%+v", l)
	}
}

// TestParseJSONLine_UpstreamRetryList 多次 upstream 尝试时 nginx 输出逗号分隔列表。
func TestParseJSONLine_UpstreamRetryList(t *testing.T) {
	raw := `{"time":"2026-09-08T13:20:03+08:00","status":"200",` +
		`"upstream_addr":"10.0.1.11:8080, 10.0.1.12:8080","upstream_status":"502, 200",` +
		`"upstream_rt":"0.002, 0.031","request_rt":"0.040","bytes":"512","ua":"-"}`
	l := ParseLine(raw, "rs2")
	if l.UpstreamRT != 0.002 {
		t.Fatalf("upstream_rt 应取首跳 0.002，got %v", l.UpstreamRT)
	}
	// 原始列表对定位「重试到第二台」有价值，字符串字段保持原样不截断。
	if l.UpstreamStatus != "502, 200" {
		t.Fatalf("upstream_status 应保留完整列表，got %q", l.UpstreamStatus)
	}
}

// TestParseJSONLine_BareNumbersStillWork 兼容用户自定义的裸数字 JSON 格式
// （非 T060 下发，但线上可能已存在）。
func TestParseJSONLine_BareNumbersStillWork(t *testing.T) {
	raw := `{"time":"2026-09-08T13:20:04+08:00","status":500,"bytes":77,` +
		`"request_rt":1.5,"upstream_rt":1.4,"uri":"/x"}`
	l := ParseLine(raw, "rs1")
	if l.Status != 500 || l.Bytes != 77 || l.RequestRT != 1.5 || l.UpstreamRT != 1.4 {
		t.Fatalf("裸数字格式解析错：%+v", l)
	}
}

// TestParseJSONLine_DirtyNumberDoesNotKillLine 单字段脏数据不应让整行报废。
func TestParseJSONLine_DirtyNumberDoesNotKillLine(t *testing.T) {
	raw := `{"time":"2026-09-08T13:20:05+08:00","status":"404","bytes":"N/A",` +
		`"request_rt":"oops","uri":"/missing"}`
	l := ParseLine(raw, "rs1")
	if l.Status != 404 || l.URI != "/missing" {
		t.Fatalf("脏数值字段不应影响其它字段：%+v", l)
	}
	if l.Bytes != 0 || l.RequestRT != 0 {
		t.Fatalf("脏数值应退化为 0，got bytes=%d rt=%v", l.Bytes, l.RequestRT)
	}
}

func TestUnquoteNum(t *testing.T) {
	cases := map[string]string{
		`"123"`:          "123",
		`123`:            "123",
		`""`:             "",
		`"-"`:            "",
		`null`:           "",
		`"0.002, 0.031"`: "0.002",
		`" 42 "`:         "42",
	}
	for in, want := range cases {
		if got := unquoteNum([]byte(in)); got != want {
			t.Fatalf("unquoteNum(%s)=%q，期望 %q", in, got, want)
		}
	}
}
