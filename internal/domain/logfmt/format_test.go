// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package logfmt

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/th/ngxcp/internal/agent/logtail"
)

func TestRenderSnippet_Contract(t *testing.T) {
	var o SnippetOptions
	o.LogPath = "/var/log/nginx/ngxcp-access.json.log"
	o.Normalize()
	if err := o.Validate(); err != nil {
		t.Fatalf("默认参数应合法：%v", err)
	}
	s := RenderSnippet(o)

	if !strings.Contains(s, "escape=json") {
		t.Fatal("必须带 escape=json，否则 UA/URI 含引号会破坏 JSON")
	}
	if !strings.Contains(s, "log_format ngxcp_json escape=json") {
		t.Fatalf("log_format 声明缺失：\n%s", s)
	}
	// 每个字段的值都必须带引号：空变量（无 upstream）时才不会产出非法 JSON。
	for _, f := range jsonFields {
		want := fmt.Sprintf(`'"%s":"%s"`, f.Key, f.Var)
		if !strings.Contains(s, want) {
			t.Fatalf("字段 %s 未按 \"key\":\"$var\" 渲染（裸变量会在空值时破坏 JSON）：\n%s", f.Key, s)
		}
	}
	// 末字段不得带逗号。
	last := jsonFields[len(jsonFields)-1]
	if strings.Contains(s, fmt.Sprintf(`'"%s":"%s",'`, last.Key, last.Var)) {
		t.Fatal("末字段带了逗号，JSON 非法")
	}
	if !strings.Contains(s, "access_log /var/log/nginx/ngxcp-access.json.log ngxcp_json buffer=32k flush=5s;") {
		t.Fatalf("access_log 行不符合预期：\n%s", s)
	}
	if !strings.HasPrefix(s, "# 本文件由 NGX-CP 自动下发") {
		t.Fatal("片段应以来源说明注释开头，便于运维辨识")
	}
}

// TestFieldKeys_MatchAgentContract 锁住 T060↔T061 的字段契约：
// 下发格式的每个 key 都必须能被 Agent 的 LogLine 接住，否则日志入库后字段全空。
func TestFieldKeys_MatchAgentContract(t *testing.T) {
	tags := map[string]bool{}
	rt := reflect.TypeOf(logtail.LogLine{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		tags[strings.SplitN(tag, ",", 2)[0]] = true
	}
	for _, k := range FieldKeys() {
		if !tags[k] {
			t.Fatalf("下发字段 %q 在 logtail.LogLine 中无对应 json tag —— 采集侧会解析为空值", k)
		}
	}
}

// renderNginxLine 模拟 nginx 按 T060 格式输出一行：变量取值来自 vars，
// 未提供的变量为空串（真实场景中 $upstream_* 在无后端时就是空）。
func renderNginxLine(vars map[string]string) string {
	parts := make([]string, 0, len(jsonFields))
	for _, f := range jsonFields {
		parts = append(parts, fmt.Sprintf("%q:%q", f.Key, vars[f.Var]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestRenderedFormat_ParsedByAgent(t *testing.T) {
	line := renderNginxLine(map[string]string{
		"$time_iso8601":           "2026-09-08T13:20:01+08:00",
		"$request_id":             "9f1c7a2b",
		"$remote_addr":            "203.0.113.10",
		"$server_name":            "a.example.com",
		"$request_uri":            "/api/v1/orders?page=2",
		"$status":                 "200",
		"$upstream_addr":          "10.0.1.11:8080",
		"$upstream_status":        "200",
		"$upstream_response_time": "0.031",
		"$request_time":           "0.033",
		"$body_bytes_sent":        "1543",
		"$http_user_agent":        "curl/8.6.0",
	})
	l := logtail.ParseLine(line, "rs1")
	if l.Status != 200 || l.Bytes != 1543 || l.UpstreamRT != 0.031 || l.RequestRT != 0.033 {
		t.Fatalf("数值字段解析错：%+v\n行：%s", l, line)
	}
	if l.RID != "9f1c7a2b" || l.URI != "/api/v1/orders?page=2" || l.UpstreamAddr != "10.0.1.11:8080" {
		t.Fatalf("字符串字段解析错：%+v", l)
	}
	if l.TS != "2026-09-08T13:20:01+08:00" {
		t.Fatalf("时间应为 ISO8601 原样透传，got %q", l.TS)
	}

	// 无 upstream 的静态请求：三个 upstream 变量为空，整行仍须解析成功。
	line = renderNginxLine(map[string]string{
		"$time_iso8601":    "2026-09-08T13:20:02+08:00",
		"$request_uri":     "/static/app.css",
		"$status":          "304",
		"$request_time":    "0.000",
		"$body_bytes_sent": "0",
	})
	l = logtail.ParseLine(line, "rs1")
	if l.Status != 304 || l.URI != "/static/app.css" {
		t.Fatalf("空 upstream 场景整行解析失败（这是裸变量格式的典型症状）：%+v\n行：%s", l, line)
	}
}

// TestSampleJSONL_Parsable 用仓库内的样例日志做回归：任何格式改动若破坏解析，这里先炸。
func TestSampleJSONL_Parsable(t *testing.T) {
	f, err := os.Open("../../../testdata/access_log_sample.jsonl")
	if err != nil {
		t.Fatalf("读取样例日志失败：%v", err)
	}
	defer f.Close()

	var n, errClass, withUpstream int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		n++
		l := logtail.ParseLine(raw, "rs1")
		if l.Status == 0 {
			t.Fatalf("第 %d 行解析失败（status=0）：%s", n, raw)
		}
		if l.TS == "" {
			t.Fatalf("第 %d 行缺时间：%s", n, raw)
		}
		if l.IsErrorClass() {
			errClass++
		}
		if l.UpstreamAddr != "" {
			withUpstream++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("扫描样例失败：%v", err)
	}
	if n < 5 {
		t.Fatalf("样例行数太少（%d），覆盖不足", n)
	}
	if errClass == 0 || withUpstream == 0 {
		t.Fatalf("样例需同时覆盖错误类与带 upstream 的请求，got err=%d upstream=%d", errClass, withUpstream)
	}
}

func TestSupportsEscapeJSON(t *testing.T) {
	cases := map[string]bool{
		"1.30.0":       true,
		"1.11.8":       true,
		"1.11.7":       false,
		"1.10.3":       false,
		"1.20.1":       true,
		"1.25.3-1.el9": true,
		"":             false,
		"abc":          false,
		"2.0":          true,
	}
	for v, want := range cases {
		if got := SupportsEscapeJSON(v); got != want {
			t.Fatalf("SupportsEscapeJSON(%q)=%v，期望 %v", v, got, want)
		}
	}
}

// TestSnippetOptions_Validate 防注入：用户可自定义日志路径/格式名，
// 必须挡住能改变 nginx 指令语义的字符。
func TestSnippetOptions_Validate(t *testing.T) {
	bad := []SnippetOptions{
		{FormatName: "ngxcp json", LogPath: "/var/log/nginx/a.log"},
		{FormatName: "ngxcp_json", LogPath: "relative/a.log"},
		{FormatName: "ngxcp_json", LogPath: "/var/log/a.log; deny all"},
		{FormatName: "ngxcp_json", LogPath: "/var/log/$host.log"},
		{FormatName: "ngxcp_json", LogPath: "/var/log/../../etc/shadow"},
		{FormatName: "ngxcp_json", LogPath: "/var/log/a.log", Buffer: "32kk"},
		{FormatName: "ngxcp_json", LogPath: "/var/log/a.log", Flush: "5x"},
	}
	for i, o := range bad {
		if err := o.Validate(); err == nil {
			t.Fatalf("第 %d 组非法参数应被拒绝：%+v", i, o)
		}
	}
	ok := SnippetOptions{FormatName: "ngxcp_json", LogPath: "/var/log/nginx/x.json.log", Buffer: "64k", Flush: "1m"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法参数被拒：%v", err)
	}
}
