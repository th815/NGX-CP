// Package logtail 实现 Agent 侧 Nginx 访问日志采集模块。
//
// 设计要点（见 docs/tasks/M6-logs-security.md T061）：
//   - 从持久化的 offset 续读，应对 Agent 重启不丢行；
//   - 监控 inode 变化应对 logrotate（原 fd 读完 EOF 后重开 path）；
//   - 同 inode 出现截断（size < offset）则重置 offset=0；
//   - 高负载时按 SampleRate 降采样，但 4xx/5xx 永不丢（安全相关）；
//   - emit 失败进入本地磁盘队列 store-forward（保留 24h，启动时回放）；
//   - offset 用 tmp+rename 原子写，避免写一半崩溃。
//
// 本包只负责"把日志行可靠地喂给 emit"，不感知下游（下游可能是
// 上报控制面再入库 ClickHouse，由 T062 实现）。emit 是可注入函数，
// 因此可无真机/无 ClickHouse 单测。
//
// 解析（T060-补，2026-09-08）：Nginx 访问日志实际形态有两种——
//   - JSON 格式（T060 下发标准 log_format 后）；
//   - 标准文本格式（nginx 默认 combined 及其 upstream 变体，绝大多数存量
//     业务在用，且无法随意改动线上格式）。
//
// 故 ParseLine 做自动探测：以 '{' 开头按 JSON 解析，否则按标准文本正则解析。
// 标准 combined 的 $time_local 是 CLF 形态（02/Jan/2006:15:04:05 +0800），
// 解析后统一转换为 RFC3339 存入 TS，保证控制面 DefaultParseTS 与时间窗查询一致。
package logtail

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// clfTimeLayout 是 nginx $time_local 的标准 CLF 时间布局。
const clfTimeLayout = "02/Jan/2006:15:04:05 -0700"

// LogLine 是解析自 Nginx 访问日志的一行，支持 JSON 或标准文本格式。
// 字段名与 T060 下发的 log_format 键保持一致（文本格式解析时按位置映射）。
type LogLine struct {
	TS             string  `json:"time"`          // RFC3339(Nano)；JSON 取 $time_iso8601，文本由 $time_local 转换
	Node           string  `json:"node"`          // 注入的节点标识（采集侧填）
	RID            string  `json:"rid"`           // $request_id / $http_x_request_id（TraceID）
	RemoteAddr     string  `json:"remote_addr"`   // $remote_addr
	Server         string  `json:"server"`        // $server_name
	URI            string  `json:"uri"`           // $request_uri / $request 中的 URI 段
	Status         int     `json:"status"`        // $status
	UpstreamAddr   string  `json:"upstream_addr"` // $upstream_addr
	UpstreamStatus string  `json:"upstream_status"`
	UpstreamRT     float32 `json:"upstream_rt"` // $upstream_response_time（取首个）
	RequestRT      float32 `json:"request_rt"`  // $request_time
	Bytes          uint32  `json:"bytes"`       // $body_bytes_sent
	UA             string  `json:"ua"`          // $http_user_agent

	Raw string `json:"-"` // 原始行，便于回放/证据留存
}

// jsonWire 是 LogLine 在 JSON 日志中的子集（Raw 不入 JSON）。
//
// 数值字段用 flex* 类型而非原生 int/float32：T060 下发的 log_format 把所有值都加了
// 引号（否则无 upstream 的请求会渲染出非法 JSON，见 flexnum.go 的说明），因此这里
// 必须同时吃下 `123` 与 `"123"`、`""`、`"0.002, 0.031"` 四种形态。用原生类型会让
// 整行 Unmarshal 失败 → 字段全零 → 表现为「格式下发成功但平台查不到数据」。
type jsonWire struct {
	TS             string      `json:"time"`
	RID            string      `json:"rid"`
	RemoteAddr     string      `json:"remote_addr"`
	Server         string      `json:"server"`
	URI            string      `json:"uri"`
	Status         flexInt     `json:"status"`
	UpstreamAddr   string      `json:"upstream_addr"`
	UpstreamStatus string      `json:"upstream_status"`
	UpstreamRT     flexFloat32 `json:"upstream_rt"`
	RequestRT      flexFloat32 `json:"request_rt"`
	Bytes          flexUint32  `json:"bytes"`
	UA             string      `json:"ua"`
}

// ParseLine 解析单行访问日志，自动探测格式：以 '{' 开头按 JSON 解析，
// 其余按标准 Nginx 文本格式（combined 及 upstream 变体）正则解析。
// 解析失败不返回 error（日志行可能因 escape 异常损坏），而是返回 Raw 已填、
// 其余字段为零值的 LogLine，由上层决定是否保留。node 由采集侧在 Tailer 上设置。
func ParseLine(raw string, node string) LogLine {
	if strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return parseJSONLine(raw, node)
	}
	return parseStandardLine(raw, node)
}

// parseJSONLine 解析单行 JSON 访问日志（T060 契约）。
func parseJSONLine(raw string, node string) LogLine {
	l := LogLine{Raw: raw, Node: node}
	var w jsonWire
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return l
	}
	l.TS = w.TS
	l.RID = w.RID
	l.RemoteAddr = w.RemoteAddr
	l.Server = w.Server
	l.URI = w.URI
	l.Status = int(w.Status)
	l.UpstreamAddr = w.UpstreamAddr
	l.UpstreamStatus = w.UpstreamStatus
	l.UpstreamRT = float32(w.UpstreamRT)
	l.RequestRT = float32(w.RequestRT)
	l.Bytes = uint32(w.Bytes)
	l.UA = w.UA
	return l
}

// combinedRe 匹配标准 nginx combined 文本格式的核心部分：
//
//	$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent"
//
// 末尾 (?P<rest>.*) 捕获可能的附加字段（见 trailingRe）。
var combinedRe = regexp.MustCompile(`^(?P<remote>\S+) (?P<ident>\S+) (?P<user>\S+) \[(?P<time>[^\]]+)\] "(?P<request>[^"]*)" (?P<status>\d{3}) (?P<bytes>\d+|-) "(?P<referer>[^"]*)" "(?P<ua>[^"]*)"(?P<rest>.*)$`)

// trailingRe 匹配 combined 核心之后的常见运维后缀（一个或多个）：
//
//	$request_time $upstream_addr $upstream_status $upstream_response_time [$request_id]
//
// $upstream_response_time 可能是逗号分隔列表（多 upstream 尝试），故 urt 用惰性匹配，
// 末尾可选 rid 作为最后一个 token。
var trailingRe = regexp.MustCompile(`^\s*(?P<rt>[\d.]+)\s+(?P<upstream>\S+)\s+(?P<ustatus>\S+)\s+(?P<urt>[\d., ]+?)(?:\s+(?P<rid>\S+))?\s*$`)

// parseStandardLine 解析标准 Nginx 文本访问日志（非 JSON）。
// 匹配不上 combined 格式时（如 error_log、脏数据）返回 Raw 已填、Status 0 的 LogLine，
// 由上层按"无法解析不丢"策略处理。
func parseStandardLine(raw string, node string) LogLine {
	l := LogLine{Raw: raw, Node: node}
	s := strings.TrimRight(raw, "\r\n")
	m := combinedRe.FindStringSubmatch(s)
	if m == nil {
		return l
	}
	gi := func(name string) string { return m[combinedRe.SubexpIndex(name)] }

	// 时间：$time_local(CLF) → RFC3339，保证控制面时间窗查询一致。
	if tl := gi("time"); tl != "" {
		if t, err := time.Parse(clfTimeLayout, tl); err == nil {
			l.TS = t.Format(time.RFC3339Nano)
		}
	}
	l.RemoteAddr = gi("remote")
	if req := gi("request"); req != "" {
		if parts := strings.SplitN(req, " ", 3); len(parts) >= 2 {
			l.URI = parts[1]
		}
	}
	if st := gi("status"); st != "" {
		if v, err := strconv.Atoi(st); err == nil {
			l.Status = v
		}
	}
	if b := gi("bytes"); b != "" && b != "-" {
		if v, err := strconv.ParseUint(b, 10, 32); err == nil {
			l.Bytes = uint32(v)
		}
	}
	l.UA = gi("ua")

	// 可选尾巴：request_time / upstream_addr / upstream_status / upstream_rt / request_id
	// 没有则直接返回（纯 combined）。
	rest := strings.TrimSpace(gi("rest"))
	if rest == "" {
		return l
	}
	fields := strings.Fields(rest)
	// 仅一个浮点 token：视为 $request_time（部分格式只附加了它）。
	if len(fields) == 1 {
		if v, err := strconv.ParseFloat(fields[0], 32); err == nil {
			l.RequestRT = float32(v)
		}
		return l
	}
	if tm := trailingRe.FindStringSubmatch(rest); tm != nil {
		tgi := func(name string) string { return tm[trailingRe.SubexpIndex(name)] }
		if rt := tgi("rt"); rt != "" {
			if v, err := strconv.ParseFloat(rt, 32); err == nil {
				l.RequestRT = float32(v)
			}
		}
		l.UpstreamAddr = tgi("upstream")
		l.UpstreamStatus = tgi("ustatus")
		if urt := tgi("urt"); urt != "" {
			if first := strings.TrimSpace(strings.SplitN(urt, ",", 2)[0]); first != "" {
				if v, err := strconv.ParseFloat(first, 32); err == nil {
					l.UpstreamRT = float32(v)
				}
			}
		}
		l.RID = tgi("rid")
	}
	return l
}

// IsErrorClass 判定是否为安全/排障需保留的类别（4xx/5xx）。
// 降采样时这类行永不丢弃。
func (l LogLine) IsErrorClass() bool {
	return l.Status >= 400
}

// sampleKeep 在降采样下决定是否保留该行。以下两类恒保留：
//   - error 类（4xx/5xx）；
//   - 无法解析（status==0），保留以便排查而非静默丢弃。
//
// 其余（2xx/3xx）按 rate 概率保留（r ∈ [0,1)）。rate>=1 表示全采。
func sampleKeep(l LogLine, rate float64, rng func() float64) bool {
	if l.IsErrorClass() || l.Status == 0 {
		return true
	}
	if rate >= 1 {
		return true
	}
	if rate <= 0 {
		return false
	}
	return rng() < rate
}

// trimNewline 去掉行尾的 \n / \r\n。
func trimNewline(s string) string {
	return strings.TrimRight(s, "\r\n")
}
