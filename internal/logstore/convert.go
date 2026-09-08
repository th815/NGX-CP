package logstore

import (
	"time"

	"github.com/th/ngxcp/internal/agent/logtail"
)

// DefaultParseTS 解析 Nginx $time_iso8601 产生的 RFC3339(Nano) 字符串。
// 例如 "2026-09-08T10:00:00+08:00"。
func DefaultParseTS(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// FromLogLines 将 Agent 采集的 LogLine 转为规范化 Entry（供入库）。
// 解析失败的 TS 退化为零值（ClickHouse 落 epoch 1970），不丢行。
// parseTS 为 nil 时使用 DefaultParseTS。
func FromLogLines(lines []logtail.LogLine, parseTS func(string) (time.Time, error)) []Entry {
	if parseTS == nil {
		parseTS = DefaultParseTS
	}
	out := make([]Entry, 0, len(lines))
	for _, l := range lines {
		ts, err := parseTS(l.TS)
		if err != nil {
			ts = time.Time{}
		}
		out = append(out, Entry{
			TS:             ts,
			Node:           l.Node,
			RID:            l.RID,
			RemoteAddr:     l.RemoteAddr,
			Server:         l.Server,
			URI:            l.URI,
			Status:         uint16(clampStatus(l.Status)),
			UpstreamAddr:   l.UpstreamAddr,
			UpstreamStatus: l.UpstreamStatus,
			UpstreamRT:     l.UpstreamRT,
			RequestRT:      l.RequestRT,
			Bytes:          l.Bytes,
			UA:             l.UA,
			Raw:            l.Raw,
		})
	}
	return out
}

// clampStatus 把 int 状态收敛到 UInt16 范围。
func clampStatus(s int) int {
	if s < 0 {
		return 0
	}
	if s > 65535 {
		return 65535
	}
	return s
}
