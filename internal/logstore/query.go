package logstore

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// QueryParams 是日志检索的多维筛选条件（见 T063 契约）。
// 所有字段均为可选；空表示不限制。时间窗缺省时回落近 24h，避免全表扫。
type QueryParams struct {
	TimeFrom time.Time // 含下界
	TimeTo   time.Time // 含上界
	Nodes    []string  // 节点名（IN）
	Status   []uint16  // HTTP 状态码（IN）
	URI      string    // 请求路径（regex=false 子串；regex=true 正则）
	IP       string    // 客户端地址（精确 =）
	RID      string    // TraceID/request_id（精确 =）
	RTMin    float32   // request_rt 下限（秒）
	Regex    bool      // URI 是否按正则匹配
	Page     int       // 从 1 起
	Size     int       // 每页条数（默认 50，上限 200）
}

// QueryResult 是检索结果。
type QueryResult struct {
	Items  []Entry
	Total  int64 // 满足筛选的总条数（不受分页影响）
	TookMs int64 // 查询耗时（毫秒）
}

// normalize 补全分页默认值并施加默认时间窗（近 24h），防止越界与全表扫。
func (p QueryParams) normalize() QueryParams {
	out := p
	if out.Size <= 0 {
		out.Size = 50
	}
	if out.Size > 200 {
		out.Size = 200
	}
	if out.Page <= 0 {
		out.Page = 1
	}
	now := time.Now()
	switch {
	case out.TimeFrom.IsZero() && out.TimeTo.IsZero():
		out.TimeTo = now
		out.TimeFrom = now.Add(-24 * time.Hour)
	case out.TimeFrom.IsZero():
		out.TimeFrom = out.TimeTo.Add(-24 * time.Hour)
	case out.TimeTo.IsZero():
		out.TimeTo = now
	}
	return out
}

// buildWhere 生成 WHERE 片段（不含 "WHERE" 关键字）与参数列表。
// 全部用户输入走 ? 占位参数，绝不做字符串拼接，从根上杜绝 SQL 注入。
// 同一构造供 ClickHouse 的「数据查询」与「计数查询」复用，保证语义一致。
func buildWhere(p QueryParams) (string, []any) {
	var conds []string
	var args []any

	conds = append(conds, "ts >= ?", "ts <= ?")
	args = append(args, p.TimeFrom, p.TimeTo)

	if len(p.Nodes) > 0 {
		conds = append(conds, fmt.Sprintf("node IN (%s)", placeholders(len(p.Nodes))))
		for _, n := range p.Nodes {
			args = append(args, n)
		}
	}
	if len(p.Status) > 0 {
		conds = append(conds, fmt.Sprintf("status IN (%s)", placeholders(len(p.Status))))
		for _, s := range p.Status {
			args = append(args, s)
		}
	}
	if p.RID != "" {
		conds = append(conds, "rid = ?")
		args = append(args, p.RID)
	}
	if p.IP != "" {
		conds = append(conds, "remote_addr = ?")
		args = append(args, p.IP)
	}
	if p.URI != "" {
		if p.Regex {
			// 生产 ClickHouse 走 match(uri, ?)；内存实现用 Go regexp（见 match）。
			conds = append(conds, "match(uri, ?)")
			args = append(args, p.URI)
		} else {
			// regex=false：转义用户输入中的 %/_ 通配符后做 LIKE 子串匹配。
			conds = append(conds, "uri LIKE ?")
			args = append(args, "%"+escapeLike(p.URI)+"%")
		}
	}
	if p.RTMin > 0 {
		conds = append(conds, "request_rt >= ?")
		args = append(args, p.RTMin)
	}
	return strings.Join(conds, " AND "), args
}

// buildDataQuery 生成分页数据查询（ORDER BY ts DESC）。
func buildDataQuery(p QueryParams) (string, []any) {
	where, args := buildWhere(p)
	q := "SELECT ts, node, rid, remote_addr, server, uri, status, upstream_addr, " +
		"upstream_status, upstream_rt, request_rt, bytes, ua, raw " +
		"FROM nginx_access WHERE " + where + " ORDER BY ts DESC LIMIT ? OFFSET ?"
	offset := (p.Page - 1) * p.Size
	args = append(args, p.Size, offset)
	return q, args
}

// buildCountQuery 生成满足条件的计数查询。
func buildCountQuery(p QueryParams) (string, []any) {
	where, args := buildWhere(p)
	return "SELECT count() FROM nginx_access WHERE " + where, args
}

// placeholders 生成 n 个逗号分隔的 ? 占位（IN 列表用）。
func placeholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

// escapeLike 转义 LIKE 通配符，regex=false 时防止用户输入的 %/_ 被当通配。
func escapeLike(s string) string {
	return strings.NewReplacer("%", "\\%", "_", "\\_").Replace(s)
}

// MemStorage.Query 在内存中实现与 ClickHouse 一致的筛选/分页语义，便于单测与开发。
func (m *MemStorage) Query(_ context.Context, p QueryParams) (*QueryResult, error) {
	p = p.normalize()
	m.mu.Lock()
	src := make([]Entry, len(m.rows))
	copy(src, m.rows)
	m.mu.Unlock()

	filtered := make([]Entry, 0, len(src))
	for _, e := range src {
		if p.match(e) {
			filtered = append(filtered, e)
		}
	}
	total := int64(len(filtered))

	// ts 降序（与 ClickHouse ORDER BY ts DESC 一致）。
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].TS.After(filtered[j].TS)
	})

	offset := (p.Page - 1) * p.Size
	if offset > len(filtered) {
		offset = len(filtered)
	}
	end := offset + p.Size
	if end > len(filtered) {
		end = len(filtered)
	}
	page := make([]Entry, end-offset)
	copy(page, filtered[offset:end])

	return &QueryResult{Items: page, Total: total, TookMs: 0}, nil
}

// match 判断单条 Entry 是否满足筛选条件（内存实现）。
func (p QueryParams) match(e Entry) bool {
	if !p.TimeFrom.IsZero() && e.TS.Before(p.TimeFrom) {
		return false
	}
	if !p.TimeTo.IsZero() && e.TS.After(p.TimeTo) {
		return false
	}
	if len(p.Nodes) > 0 && !containsStr(p.Nodes, e.Node) {
		return false
	}
	if len(p.Status) > 0 && !containsUint16(p.Status, e.Status) {
		return false
	}
	if p.RID != "" && e.RID != p.RID {
		return false
	}
	if p.IP != "" && e.RemoteAddr != p.IP {
		return false
	}
	if p.URI != "" {
		if p.Regex {
			matched, err := regexp.MatchString(p.URI, e.URI)
			if err != nil || !matched {
				return false
			}
		} else if !strings.Contains(e.URI, p.URI) {
			return false
		}
	}
	if p.RTMin > 0 && e.RequestRT < p.RTMin {
		return false
	}
	return true
}

func containsStr(hay []string, v string) bool {
	for _, x := range hay {
		if x == v {
			return true
		}
	}
	return false
}

func containsUint16(hay []uint16, v uint16) bool {
	for _, x := range hay {
		if x == v {
			return true
		}
	}
	return false
}
