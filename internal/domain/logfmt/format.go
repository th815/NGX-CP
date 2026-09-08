// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package logfmt 实现 T060「标准 JSON log_format 下发」。
//
// 为什么需要它：T061 的采集、T063 的检索、T064 的 TraceID、T065 的聚合都要求日志里
// 有 `$request_id` / `$upstream_addr` / `$upstream_response_time` 这些字段。存量业务
// 用的是 nginx 默认 combined 文本格式，既没有 request_id（追踪永远为空），也无法保证
// 各节点格式一致。本包把统一格式做成一个**新增**的配置片段，走 M3 变更单流水线下发。
//
// 三条设计红线（都踩过或差点踩）：
//
//  1. **只增不改**：不改写用户现有的 nginx.conf / access_log 指令。片段是独立文件
//     `<include_dir>/zz-ngxcp-logformat.conf`，只声明一个新 log_format 与一条**额外**的
//     access_log。nginx 允许同层级多条 access_log 并行写入，故存量文本日志照旧产出，
//     业务零感知；代价是磁盘多一份日志（Plan 会显式告警）。
//
//  2. **所有 JSON 值一律加引号**。契约草案里 `"upstream_rt":$upstream_response_time`
//     是错的：无 upstream 的请求（静态文件、redirect、error_page）该变量为空串，渲染出
//     `"upstream_rt":,` —— 非法 JSON，整行报废。统一加引号后 nginx 永远产出合法 JSON，
//     代价是数值变字符串，因此 logtail 侧的解析必须容忍带引号数字（见 line.go 的
//     flex* 类型，与本包同一次改动落地）。
//
//  3. **escape=json 必须加**，否则 UA / URI 里的双引号和控制字符会破坏 JSON。该参数需
//     nginx ≥ 1.11.8，故下发前做版本门槛检查，而不是发完等 `nginx -t` 报错。
package logfmt

import (
	"fmt"
	"strconv"
	"strings"
)

// 下发契约的默认值。
const (
	DefaultFormatName = "ngxcp_json"              // log_format 名称
	DefaultLogFile    = "ngxcp-access.json.log"   // 新增日志文件名（与存量 access.log 区分）
	DefaultLogDir     = "/var/log/nginx"          // 无法从能力基线推导时的兜底目录
	SnippetFileName   = "zz-ngxcp-logformat.conf" // zz 前缀保证在 include 目录内最后加载
	DefaultBuffer     = "32k"                     // 攒批写盘，降低高 QPS 下的 IO 抖动
	DefaultFlush      = "5s"                      // 缓冲最长滞留时间（影响日志可见延迟）
	MinNginxVersion   = "1.11.8"                  // escape=json 引入版本
)

// snippetHeader 是片段顶部的说明注释（运维直接 cat 就知道这文件是谁写的、能不能删）。
const snippetHeader = `# 本文件由 NGX-CP 自动下发（T060 标准 JSON 日志格式），请勿手工编辑。
# 手工修改会在下次配置巡检中被判为配置漂移。
#
# 作用：在**不改动**任何现有 access_log 的前提下，额外输出一份结构化 JSON 日志，
#      供平台做统一检索（T063）、TraceID 全链路追踪（T064）与聚合分析（T065）。
# 影响：nginx 同层级的多条 access_log 会并行写入，故原有日志与格式保持原样不变；
#      该节点会同时写两份日志，磁盘占用相应增加，请确认 logrotate 已覆盖新文件。
# 注意：server{} / location{} 内如已有自己的 access_log，会就近覆盖本文件的 http 级
#      设置，那些站点需要单独下发（平台会在下发预览里列出）。`

// jsonField 是下发格式的一个字段：Key 必须与 logtail.LogLine 的 json tag 完全一致，
// 否则 Agent 解析出来全是零值（这正是「下发成功但查不到数据」的典型成因）。
type jsonField struct {
	Key string
	Var string
}

// jsonFields 是字段清单（顺序即日志中的顺序）。
//
// 与 logtail.LogLine 的对应关系由 logfmt_test.go 的契约测试锁死：
// 任何一方改字段名，测试立刻失败。
var jsonFields = []jsonField{
	{"time", "$time_iso8601"},                  // 带时区的 ISO8601，控制面直接解析
	{"rid", "$request_id"},                     // ★ TraceID：跨节点串联一次请求
	{"remote_addr", "$remote_addr"},            // 客户端 IP（DR 模式下为真实客户端）
	{"server", "$server_name"},                 // 命中的 server_name
	{"uri", "$request_uri"},                    // 含 query 的原始 URI
	{"status", "$status"},                      // HTTP 状态码
	{"upstream_addr", "$upstream_addr"},        // ★ 落到哪个后端（2 RS 场景定位关键）
	{"upstream_status", "$upstream_status"},    // 后端返回码（与 status 不同即 nginx 改写过）
	{"upstream_rt", "$upstream_response_time"}, // ★ 慢在后端
	{"request_rt", "$request_time"},            // ★ 慢在整体（含 nginx 自身与网络）
	{"bytes", "$body_bytes_sent"},              // 响应体字节数
	{"ua", "$http_user_agent"},                 // UA（扫描器指纹依据，T066）
}

// SnippetOptions 是片段渲染参数，零值经 Normalize 后即为推荐默认值。
type SnippetOptions struct {
	FormatName string // log_format 名称，默认 ngxcp_json
	LogPath    string // 新增日志文件绝对路径，默认 <推导目录>/ngxcp-access.json.log
	Buffer     string // access_log buffer=，空串表示不加
	Flush      string // access_log flush=，空串表示不加
}

// Normalize 补齐默认值。LogPath 为空时由调用方（Plan）按能力基线推导后再传入，
// 此处仅兜底到 DefaultLogDir。
func (o *SnippetOptions) Normalize() {
	if o.FormatName == "" {
		o.FormatName = DefaultFormatName
	}
	if o.LogPath == "" {
		o.LogPath = DefaultLogDir + "/" + DefaultLogFile
	}
	if o.Buffer == "" {
		o.Buffer = DefaultBuffer
	}
	if o.Flush == "" {
		o.Flush = DefaultFlush
	}
}

// Validate 校验参数（防止把用户输入直接拼进 nginx 配置）。
func (o SnippetOptions) Validate() error {
	if !isSafeIdent(o.FormatName) {
		return fmt.Errorf("format_name 只允许字母/数字/下划线：%q", o.FormatName)
	}
	if !isSafeAbsPath(o.LogPath) {
		return fmt.Errorf("log_path 必须是不含空白与特殊字符的绝对路径：%q", o.LogPath)
	}
	if o.Buffer != "" && !isSafeSize(o.Buffer) {
		return fmt.Errorf("buffer 形如 32k / 1m：%q", o.Buffer)
	}
	if o.Flush != "" && !isSafeTime(o.Flush) {
		return fmt.Errorf("flush 形如 5s / 1m：%q", o.Flush)
	}
	return nil
}

// RenderSnippet 渲染 log_format 片段。调用前应已 Normalize + Validate。
func RenderSnippet(o SnippetOptions) string {
	var b strings.Builder
	b.WriteString(snippetHeader)
	b.WriteString("\n\n")

	// log_format：每个字段单独一行，便于 diff 时精确定位改了哪个字段。
	fmt.Fprintf(&b, "log_format %s escape=json\n", o.FormatName)
	b.WriteString("  '{'\n")
	for i, f := range jsonFields {
		sep := ","
		if i == len(jsonFields)-1 {
			sep = "" // 末字段不带逗号，否则 JSON 非法
		}
		// 值统一加引号：空变量（无 upstream 时）也能产出合法 JSON。
		fmt.Fprintf(&b, "    '\"%s\":\"%s\"%s'\n", f.Key, f.Var, sep)
	}
	b.WriteString("  '}';\n\n")

	// access_log：额外一条，不触碰既有指令。
	b.WriteString("access_log " + o.LogPath + " " + o.FormatName)
	if o.Buffer != "" {
		b.WriteString(" buffer=" + o.Buffer)
	}
	if o.Flush != "" {
		b.WriteString(" flush=" + o.Flush)
	}
	b.WriteString(";\n")
	return b.String()
}

// FieldKeys 返回字段 key 列表（供契约测试与 API 预览展示）。
func FieldKeys() []string {
	out := make([]string, 0, len(jsonFields))
	for _, f := range jsonFields {
		out = append(out, f.Key)
	}
	return out
}

// SupportsEscapeJSON 判断 nginx 版本是否支持 escape=json（≥ 1.11.8）。
// 版本串取自 `nginx -V`（如 "1.30.0"）；无法解析视为不支持，宁可挡住也不要发出去炸配置。
func SupportsEscapeJSON(version string) bool {
	return compareVersion(version, MinNginxVersion) >= 0
}

// compareVersion 比较点分版本号，返回 -1 / 0 / 1；无法解析的一侧视为更小。
// 只处理数字段（1.30.0），忽略后缀（1.25.3-1.el9 → 1.25.3）。
func compareVersion(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parseVersion 把版本串解析为 3 段数字，缺失段补 0，非法段视为 -1（排序上最小）。
func parseVersion(v string) [3]int {
	out := [3]int{-1, -1, -1}
	v = strings.TrimSpace(v)
	if v == "" {
		return out
	}
	segs := strings.SplitN(v, ".", 4)
	for i := 0; i < 3; i++ {
		if i >= len(segs) {
			out[i] = 0
			continue
		}
		// 去掉数字之后的后缀（如 "3-1.el9" → "3"）。
		s := segs[i]
		j := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		n, err := strconv.Atoi(s[:j])
		if err != nil {
			return [3]int{-1, -1, -1}
		}
		out[i] = n
	}
	return out
}

// isSafeIdent 只允许字母数字下划线（nginx 标识符）。
func isSafeIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// isSafeAbsPath 只允许绝对路径，且不含空白、引号、分号、`$`、`#`
// —— 这些字符会改变 nginx 指令语义（注入风险）。
func isSafeAbsPath(s string) bool {
	if !strings.HasPrefix(s, "/") || len(s) > 512 {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n', '\'', '"', ';', '$', '#', '{', '}', '\\':
			return false
		}
	}
	return true
}

// isSafeSize 形如 32k / 1m / 4096。
func isSafeSize(s string) bool { return matchNumSuffix(s, "kKmM") }

// isSafeTime 形如 5s / 1m / 10。
func isSafeTime(s string) bool { return matchNumSuffix(s, "smh") }

// matchNumSuffix 校验「若干数字 + 可选单个后缀字符」。
func matchNumSuffix(s, suffixes string) bool {
	if s == "" || len(s) > 8 {
		return false
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	if i == len(s) {
		return true
	}
	return i == len(s)-1 && strings.ContainsRune(suffixes, rune(s[i]))
}
