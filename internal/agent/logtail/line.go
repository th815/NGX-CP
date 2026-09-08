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
package logtail

import (
	"encoding/json"
	"strings"
)

// LogLine 是解析自 Nginx 标准 JSON 访问日志（T060 契约）的一行。
// 字段名与 T060 下发的 log_format 键一致。
type LogLine struct {
	TS             string  `json:"time"`          // $time_iso8601，保留原字符串避免时区歧义
	Node           string  `json:"node"`          // 注入的节点标识（采集侧填）
	RID            string  `json:"rid"`           // $request_id（TraceID）
	RemoteAddr     string  `json:"remote_addr"`  // $remote_addr
	Server         string  `json:"server"`        // $server_name
	URI            string  `json:"uri"`           // $request_uri
	Status         int     `json:"status"`        // $status
	UpstreamAddr   string  `json:"upstream_addr"` // $upstream_addr
	UpstreamStatus string  `json:"upstream_status"`
	UpstreamRT     float32 `json:"upstream_rt"` // $upstream_response_time
	RequestRT      float32 `json:"request_rt"`  // $request_time
	Bytes          uint32  `json:"bytes"`        // $body_bytes_sent
	UA             string  `json:"ua"`           // $http_user_agent

	Raw string `json:"-"` // 原始行，便于回放/证据留存
}

// jsonWire 是 LogLine 在 JSON 日志中的子集（Raw 不入 JSON）。
type jsonWire struct {
	TS             string  `json:"time"`
	RID            string  `json:"rid"`
	RemoteAddr     string  `json:"remote_addr"`
	Server         string  `json:"server"`
	URI            string  `json:"uri"`
	Status         int     `json:"status"`
	UpstreamAddr   string  `json:"upstream_addr"`
	UpstreamStatus string  `json:"upstream_status"`
	UpstreamRT     float32 `json:"upstream_rt"`
	RequestRT      float32 `json:"request_rt"`
	Bytes          uint32  `json:"bytes"`
	UA             string  `json:"ua"`
}

// ParseLine 解析单行 JSON 访问日志。解析失败不返回 error（日志行可能
// 因 escape 异常损坏），而是返回 Raw 已填、其余字段为零值的 LogLine，
// 由上层决定是否保留。node 由采集侧在 Tailer 上设置，不在日志内。
func ParseLine(raw string, node string) LogLine {
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
	l.Status = w.Status
	l.UpstreamAddr = w.UpstreamAddr
	l.UpstreamStatus = w.UpstreamStatus
	l.UpstreamRT = w.UpstreamRT
	l.RequestRT = w.RequestRT
	l.Bytes = w.Bytes
	l.UA = w.UA
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
