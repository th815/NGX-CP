package logstore

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AggMetric 标识聚合指标（见 T065 契约）。
type AggMetric string

const (
	AggTopURI       AggMetric = "top_uri"
	AggTopIP        AggMetric = "top_ip"
	AggTopUA        AggMetric = "top_ua"
	AggStatusDist   AggMetric = "status_dist"
	AggRTPercentile AggMetric = "rt_percentile"
)

// ValidAggMetric 校验指标是否受支持（导出供 handler 使用）。
func ValidAggMetric(m AggMetric) bool {
	switch m {
	case AggTopURI, AggTopIP, AggTopUA, AggStatusDist, AggRTPercentile:
		return true
	}
	return false
}

// AggParams 是聚合分析的请求参数（见 T065 契约）。
// 时间窗经 Window 给定（"1h"/"24h"/"7d"，缺省 24h）；其余为可选筛选，
// 全部复用 QueryParams 的语义与参数化防注入逻辑。
type AggParams struct {
	Metric AggMetric
	Window string   // "1h" | "24h" | "7d"，缺省 24h
	Nodes  []string // 节点名（IN）
	Status []uint16 // 状态码（IN）
	URI    string   // 路径（子串/正则，取决于 Regex）
	IP     string   // 客户端地址（精确）
	RID    string   // TraceID（精确）
	RTMin  float32  // request_rt 下限（秒）
	Regex  bool     // URI 是否按正则
	TopN   int      // top_* 返回条数上限，缺省 10
	Limit  int      // 扫描行数上限（防止海量），缺省 100000
}

// AggRow 是聚合结果的一行。
type AggRow struct {
	Key   string  // 分组键：uri/ip/ua/状态码字符串；rt_percentile 时为 "request_rt"
	Count int64   // 该组命中条数
	Err   int64   // 该组中 status>=400 的条数（top_* 指标有意义）
	P50   float64 // request_rt 的 P50（秒），仅 rt_percentile 填充
	P95   float64
	P99   float64
}

// AggResult 是聚合分析结果。
type AggResult struct {
	Metric AggMetric
	Rows   []AggRow
	Total  int64 // 参与聚合的总条数
	TookMs int64
}

// Aggregate 在存储上执行聚合分析（Storage 接口方法）。
// 两种实现均先经 Query 取过滤后行集，再调用 computeAgg——单一可测代码路径，
// 避免引入未经真机验证的 ClickHouse GROUP BY SQL（与 T062 Query 已验风格区分）。
func (m *MemStorage) Aggregate(ctx context.Context, p AggParams) (*AggResult, error) {
	return aggregateFromQuery(ctx, m, p)
}

// Aggregate 见 MemStorage.Aggregate 说明。
func (s *ClickHouseStorage) Aggregate(ctx context.Context, p AggParams) (*AggResult, error) {
	return aggregateFromQuery(ctx, s, p)
}

// aggregateFromQuery 复用 Query 的参数化过滤与时间窗，取行后在内存聚合。
func aggregateFromQuery(ctx context.Context, s Storage, p AggParams) (*AggResult, error) {
	win := parseWindow(p.Window)
	qp := QueryParams{
		TimeFrom: time.Now().Add(-win),
		TimeTo:   time.Now(),
		Nodes:    p.Nodes,
		Status:   p.Status,
		URI:      p.URI,
		IP:       p.IP,
		RID:      p.RID,
		RTMin:    p.RTMin,
		Regex:    p.Regex,
		Page:     1,
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 100000
	}
	qp.Size = limit

	r, err := s.Query(ctx, qp)
	if err != nil {
		return nil, err
	}
	return computeAgg(r.Items, p), nil
}

// computeAgg 是聚合的核心纯函数（不依赖具体存储，便于单测）。
func computeAgg(entries []Entry, p AggParams) *AggResult {
	res := &AggResult{Metric: p.Metric, Total: int64(len(entries))}

	switch p.Metric {
	case AggRTPercentile:
		vs := make([]float64, 0, len(entries))
		for _, e := range entries {
			vs = append(vs, float64(e.RequestRT))
		}
		res.Rows = []AggRow{{
			Key:   "request_rt",
			Count: int64(len(vs)),
			P50:   percentile(vs, 0.5),
			P95:   percentile(vs, 0.95),
			P99:   percentile(vs, 0.99),
		}}

	case AggStatusDist:
		m := map[uint16]int64{}
		for _, e := range entries {
			m[e.Status]++
		}
		rows := make([]AggRow, 0, len(m))
		for s, c := range m {
			rows = append(rows, AggRow{Key: strconv.Itoa(int(s)), Count: c})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Count != rows[j].Count {
				return rows[i].Count > rows[j].Count
			}
			return rows[i].Key < rows[j].Key
		})
		res.Rows = rows

	default: // top_uri / top_ip / top_ua
		keyOf := func(e Entry) string {
			switch p.Metric {
			case AggTopIP:
				return e.RemoteAddr
			case AggTopUA:
				return e.UA
			default:
				return e.URI
			}
		}
		m := map[string][]Entry{}
		for _, e := range entries {
			k := keyOf(e)
			if k != "" {
				m[k] = append(m[k], e)
			}
		}
		rows := make([]AggRow, 0, len(m))
		for k, es := range m {
			var errN int64
			for _, e := range es {
				if e.Status >= 400 {
					errN++
				}
			}
			rows = append(rows, AggRow{Key: k, Count: int64(len(es)), Err: errN})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Count != rows[j].Count {
				return rows[i].Count > rows[j].Count
			}
			return rows[i].Key < rows[j].Key
		})
		topN := p.TopN
		if topN <= 0 {
			topN = 10
		}
		if len(rows) > topN {
			rows = rows[:topN]
		}
		res.Rows = rows
	}
	return res
}

// percentile 计算分位数（线性插值）。空输入返回 0。
func percentile(vs []float64, q float64) float64 {
	n := len(vs)
	if n == 0 {
		return 0
	}
	s := make([]float64, n)
	copy(s, vs)
	sort.Float64s(s)
	if n == 1 {
		return s[0]
	}
	idx := q * float64(n-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return s[lo]
	}
	frac := idx - float64(lo)
	return s[lo] + frac*(s[hi]-s[lo])
}

// parseWindow 解析时间窗字符串（"1h"/"24h"/"7d"）。
// Go 标准库 ParseDuration 不支持 "d"（天），故单独处理；非法/空回落 24h。
func parseWindow(w string) time.Duration {
	w = strings.TrimSpace(strings.ToLower(w))
	if w == "" {
		return 24 * time.Hour
	}
	if strings.HasSuffix(w, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(w, "d")); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
		return 24 * time.Hour
	}
	if d, err := time.ParseDuration(w); err == nil && d > 0 {
		return d
	}
	return 24 * time.Hour
}
