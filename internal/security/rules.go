// Package security 实现攻击检测规则引擎（M6 T066）。
//
// 设计要点（见 docs/tasks/M6-logs-security.md T066、docs/DECISIONS.md §3/§10）：
//   - 规则即 SQL：每条规则是一条 ClickHouse 滑动窗口查询（返回异常计数），
//     平台不在 Go 侧重写一套检测算子，直接在 CH 上跑——省事且与时序存储同源。
//   - 双轨可验：生产走 ClickHouse（CHBackend.QueryCount 参数化执行 Rule.SQL）；
//     沙箱/测试/ MemStorage 模式走 EvaluateMem（同语义内存统计 Rule.Eval）。
//     两条路径对同一组构造样本必须给出一致结论，否则测试失败——这是防
//     "规则即 SQL"退化为不可验证黑盒的关键（见 rules_test.go）。
//   - 参数化防注入：Rule.SQL 的窗口起止一律用 `?` 占位，引擎传 time.Time 绑定，
//     绝不拼接用户输入；阈值只在 Go 侧比较，不进 SQL 文本。
//   - 阈值可配：Threshold 字段而非写死；误报面大的规则默认 Action=alert，
//     仅"单 IP 高频 CC"这种极高置信规则才默认 auto（见 T069 纪律）。
package security

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/th/ngxcp/internal/logstore"
)

// 安全等级（事件分级，T067 沿用）。
const (
	LevelInfo     = "INFO"
	LevelWarn     = "WARN"
	LevelCritical = "CRITICAL"
)

// 处置动作（T069 策略语义）。
const (
	ActionAuto  = "auto"  // 高置信直接封禁（走 T068 变更单，免审批）
	ActionSemi  = "semi"  // 创建变更单等审批
	ActionAlert = "alert" // 只记录事件，人工处置
)

// EvalFunc 是规则的内存评估器：对「已在滑动窗口内」的 entries 统计异常计数，
// 并返回首个命中样本（作证据）。窗口过滤在 EvaluateMem 内完成，Eval 只吃窗口内数据。
// 其语义必须与 Rule.SQL 在 ClickHouse 上的语义一致（由测试锁死）。
type EvalFunc func(entries []*logstore.Entry, now time.Time) (count float64, sample *logstore.Entry)

// Rule 是一条攻击检测规则。
// 契约字段（来自 T066 文档）：ID/Name/Level/SQL/Window/Threshold/Action。
// Eval 为内存评估器（json:"-"：函数不可序列化，规则经 API 暴露时只给数据字段）。
type Rule struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Level     string   `json:"level"`
	Window    string   `json:"window"`    // 如 "5m"、"1m"、"24h"、"7d"
	Threshold float64  `json:"threshold"` // 异常计数阈值，>= 即触发
	Action    string   `json:"action"`    // auto | semi | alert
	SQL       string   `json:"sql"`       // ClickHouse 查询模板，含 ? 占位（窗口起止）
	Eval      EvalFunc `json:"-"`
}

// RuleHit 是某次评估的结果。
type RuleHit struct {
	RuleID      string          `json:"rule_id"`
	Name        string          `json:"name"`
	Level       string          `json:"level"`
	Action      string          `json:"action"`
	Window      string          `json:"window"`
	Threshold   float64         `json:"threshold"`
	Count       float64         `json:"count"`     // 实际异常计数
	Triggered   bool            `json:"triggered"` // Count >= Threshold
	Sample      *logstore.Entry `json:"sample,omitempty"`
	EvaluatedAt time.Time       `json:"evaluated_at"`
}

// hit 构造一次命中结果。
func (r Rule) hit(count float64, now time.Time, sample *logstore.Entry) *RuleHit {
	return &RuleHit{
		RuleID:      r.ID,
		Name:        r.Name,
		Level:       r.Level,
		Action:      r.Action,
		Window:      r.Window,
		Threshold:   r.Threshold,
		Count:       count,
		Triggered:   count >= r.Threshold,
		Sample:      sample,
		EvaluatedAt: now,
	}
}

// ParseWindow 解析窗口字符串为时长。Go 标准库不支持 "d"，单独处理。
func ParseWindow(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && strings.HasSuffix(s, "d") {
		if n <= 0 {
			return 0, fmt.Errorf("invalid window %q: must be positive", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid window %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid window %q: must be positive", s)
	}
	return d, nil
}

// ---------- 内存评估 helper（Eval 复用，保持 10 条规则紧凑） ----------

// matchAny 大小写不敏感地判断 s 是否包含任一子串。
func matchAny(s string, subs ...string) bool {
	ls := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(ls, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// countWhere 统计满足条件的条数，并返回首个命中样本。
func countWhere(es []*logstore.Entry, pred func(*logstore.Entry) bool) (int, *logstore.Entry) {
	n := 0
	var sample *logstore.Entry
	for _, e := range es {
		if pred(e) {
			n++
			if sample == nil {
				sample = e
			}
		}
	}
	return n, sample
}

// maxGroup 返回按 key 分组后最大的组计数（用于「单 IP 总请求数」类规则）。
func maxGroup(es []*logstore.Entry, key func(*logstore.Entry) string) int {
	m := make(map[string]int, len(es))
	for _, e := range es {
		m[key(e)]++
	}
	best := 0
	for _, c := range m {
		if c > best {
			best = c
		}
	}
	return best
}

// maxGroupDistinct 返回按 key 分组后，组内不同 val 数的最大值
// （用于「单 IP 爆破的不同 404 路径数」「单 IP 遍历的不同 URI 数」类规则）。
func maxGroupDistinct(es []*logstore.Entry, key func(*logstore.Entry) string, val func(*logstore.Entry) string) int {
	m := make(map[string]map[string]struct{}, len(es))
	for _, e := range es {
		k := key(e)
		if m[k] == nil {
			m[k] = make(map[string]struct{})
		}
		m[k][val(e)] = struct{}{}
	}
	best := 0
	for _, s := range m {
		if len(s) > best {
			best = len(s)
		}
	}
	return best
}

// windowEntries 过滤出 [now-window, now] 闭区间内的 entries。
func windowEntries(es []*logstore.Entry, now time.Time, window time.Duration) []*logstore.Entry {
	start := now.Add(-window)
	out := make([]*logstore.Entry, 0, len(es))
	for _, e := range es {
		if (e.TS.Equal(start) || e.TS.After(start)) && (e.TS.Before(now) || e.TS.Equal(now)) {
			out = append(out, e)
		}
	}
	return out
}

// ---------- 规则静态特征（供 SQL 与 Eval 对齐、测试断言） ----------

var (
	sqlInjectionSigs = []string{
		"union select", "or 1=1", "select from", "' or '", "-- ", "/**/",
	}
	scannerUAs = []string{
		"sqlmap", "nmap", "masscan", "nuclei", "wpscan", "dirb", "gobuster",
		"nikto", "acunetix", "burp", "zgrab", "python-requests", "python-urllib",
		"go-http-client", "java/", "libwww", "httpclient", "scanner", "fuzz",
	}
	sensitivePaths = []string{
		"/wp-admin", "/.env", "/phpmyadmin", "/admin", "/api/internal", "/.git",
		"/actuator", "/console", "/etc/passwd", "/proc/", "/boaform", "/cgi-bin",
		"/shell", "/.aws", "/config", "/setup.php", "/xmlrpc.php", "/.ssh",
	}
	oddUAs = []string{
		"curl", "wget", "python", "go-http-client", "java/", "libwww",
		"httpclient", "okhttp", "apache-httpclient", "scanner", "fuzz", "nmap",
		"masscan", "zgrab",
	}
)

// DefaultRules 返回平台内置的 10 条攻击检测规则（覆盖主要攻击面）。
// 每条规则的 SQL（生产）与 Eval（内存）语义必须一致。
func DefaultRules() []Rule {
	return []Rule{
		{
			ID: "r-sql-injection", Name: "SQL 注入特征", Level: LevelCritical,
			Window: "5m", Threshold: 1, Action: ActionSemi,
			SQL: `SELECT toFloat64(count()) FROM nginx_access
WHERE ts BETWEEN ? AND ?
  AND (positionCaseInsensitive(uri, 'union select') > 0
    OR positionCaseInsensitive(uri, 'or 1=1') > 0
    OR positionCaseInsensitive(uri, 'select from') > 0
    OR positionCaseInsensitive(uri, '' or '') > 0
    OR positionCaseInsensitive(uri, '-- ') > 0
    OR positionCaseInsensitive(uri, '/**/') > 0
    OR positionCaseInsensitive(ua, '<script') > 0)`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				n, s := countWhere(es, func(e *logstore.Entry) bool {
					return matchAny(e.URI, sqlInjectionSigs...) || matchAny(e.UA, "<script", "union select")
				})
				return float64(n), s
			},
		},
		{
			ID: "r-scanner-ua", Name: "扫描器指纹 UA", Level: LevelInfo,
			Window: "5m", Threshold: 5, Action: ActionAlert,
			SQL: `SELECT toFloat64(count()) FROM nginx_access
WHERE ts BETWEEN ? AND ?
  AND (` + scannerOR("ua") + `)`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				n, s := countWhere(es, func(e *logstore.Entry) bool {
					return matchAny(e.UA, scannerUAs...)
				})
				return float64(n), s
			},
		},
		{
			ID: "r-dir-brute", Name: "目录爆破（单 IP 大量 404 路径）", Level: LevelWarn,
			Window: "5m", Threshold: 30, Action: ActionSemi,
			SQL: `SELECT coalesce(max(c), 0) FROM (
  SELECT count(DISTINCT uri) AS c FROM nginx_access
  WHERE ts BETWEEN ? AND ? AND status = 404 GROUP BY remote_addr
)`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				var f []*logstore.Entry
				for _, e := range es {
					if e.Status == 404 {
						f = append(f, e)
					}
				}
				return float64(maxGroupDistinct(f,
					func(e *logstore.Entry) string { return e.RemoteAddr },
					func(e *logstore.Entry) string { return e.URI })), nil
			},
		},
		{
			ID: "r-cc-flood", Name: "CC 洪水（单 IP 高频请求）", Level: LevelCritical,
			Window: "1m", Threshold: 600, Action: ActionAuto,
			SQL: `SELECT coalesce(max(c), 0) FROM (
  SELECT count() AS c FROM nginx_access
  WHERE ts BETWEEN ? AND ? GROUP BY remote_addr
)`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				return float64(maxGroup(es, func(e *logstore.Entry) string { return e.RemoteAddr })), nil
			},
		},
		{
			ID: "r-slowloris", Name: "Slowloris 慢速攻击", Level: LevelWarn,
			Window: "5m", Threshold: 10, Action: ActionSemi,
			SQL: `SELECT toFloat64(count()) FROM nginx_access
WHERE ts BETWEEN ? AND ? AND request_rt > 10 AND bytes < 1024`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				n, s := countWhere(es, func(e *logstore.Entry) bool {
					return e.RequestRT > 10 && e.Bytes < 1024
				})
				return float64(n), s
			},
		},
		{
			ID: "r-5xx-spike", Name: "5xx 突增", Level: LevelWarn,
			Window: "5m", Threshold: 100, Action: ActionAlert,
			SQL: `SELECT toFloat64(count()) FROM nginx_access
WHERE ts BETWEEN ? AND ? AND status >= 500 AND status <= 599`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				n, s := countWhere(es, func(e *logstore.Entry) bool {
					return e.Status >= 500 && e.Status <= 599
				})
				return float64(n), s
			},
		},
		{
			ID: "r-4xx-spike", Name: "4xx 突增", Level: LevelInfo,
			Window: "5m", Threshold: 500, Action: ActionAlert,
			SQL: `SELECT toFloat64(count()) FROM nginx_access
WHERE ts BETWEEN ? AND ? AND status >= 400 AND status < 500`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				n, s := countWhere(es, func(e *logstore.Entry) bool {
					return e.Status >= 400 && e.Status < 500
				})
				return float64(n), s
			},
		},
		{
			ID: "r-sensitive-path", Name: "敏感路径探测", Level: LevelWarn,
			Window: "5m", Threshold: 5, Action: ActionAlert,
			SQL: `SELECT toFloat64(count()) FROM nginx_access
WHERE ts BETWEEN ? AND ? AND (` + pathOR("uri") + `)`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				n, s := countWhere(es, func(e *logstore.Entry) bool {
					return matchAny(e.URI, sensitivePaths...)
				})
				return float64(n), s
			},
		},
		{
			ID: "r-odd-ua", Name: "非常规 UA", Level: LevelInfo,
			Window: "5m", Threshold: 50, Action: ActionAlert,
			SQL: `SELECT toFloat64(count()) FROM nginx_access
WHERE ts BETWEEN ? AND ?
  AND (trim(ua) = '' OR ua = '-' OR ` + scannerOR("ua") + `)`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				n, s := countWhere(es, func(e *logstore.Entry) bool {
					ua := strings.TrimSpace(e.UA)
					return ua == "" || ua == "-" || matchAny(ua, oddUAs...)
				})
				return float64(n), s
			},
		},
		{
			ID: "r-single-ip-broad", Name: "单 IP 高频遍历（不同 URI）", Level: LevelWarn,
			Window: "5m", Threshold: 200, Action: ActionAlert,
			SQL: `SELECT coalesce(max(c), 0) FROM (
  SELECT count(DISTINCT uri) AS c FROM nginx_access
  WHERE ts BETWEEN ? AND ? GROUP BY remote_addr
)`,
			Eval: func(es []*logstore.Entry, _ time.Time) (float64, *logstore.Entry) {
				return float64(maxGroupDistinct(es,
					func(e *logstore.Entry) string { return e.RemoteAddr },
					func(e *logstore.Entry) string { return e.URI })), nil
			},
		},
	}
}

// scannerOR 生成 `positionCaseInsensitive(ua, 'x') > 0 OR ...` 片段（供 SQL 模板复用）。
func scannerOR(col string) string {
	parts := make([]string, 0, len(scannerUAs))
	for _, s := range scannerUAs {
		parts = append(parts, fmt.Sprintf("positionCaseInsensitive(%s, '%s') > 0", col, s))
	}
	return strings.Join(parts, "\n    OR ")
}

// pathOR 生成敏感路径匹配的 SQL 片段。
func pathOR(col string) string {
	parts := make([]string, 0, len(sensitivePaths))
	for _, s := range sensitivePaths {
		parts = append(parts, fmt.Sprintf("positionCaseInsensitive(%s, '%s') > 0", col, s))
	}
	return strings.Join(parts, "\n    OR ")
}

// ---------- 评估执行 ----------

// Backend 是规则计数的查询后端。生产用 CHBackend；测试用 FakeBackend。
type Backend interface {
	// QueryCount 参数化执行查询并返回计数（float64）。args 依次为窗口起止时间。
	QueryCount(ctx context.Context, query string, args ...any) (float64, error)
}

// FakeBackend 是测试用后端：固定返回预设计数或错误。
type FakeBackend struct {
	Count float64
	Err   error
}

// QueryCount 返回预设值。
func (f *FakeBackend) QueryCount(_ context.Context, _ string, _ ...any) (float64, error) {
	return f.Count, f.Err
}

// Engine 是规则评估引擎，包裹一个 Backend。
type Engine struct {
	backend Backend
}

// NewEngine 构造引擎。
func NewEngine(b Backend) *Engine { return &Engine{backend: b} }

// Evaluate 经 Backend 跑规则 SQL，返回命中结果（生产路径）。
func (e *Engine) Evaluate(ctx context.Context, rule Rule, now time.Time) (*RuleHit, error) {
	d, err := ParseWindow(rule.Window)
	if err != nil {
		return nil, err
	}
	start := now.Add(-d)
	cnt, err := e.backend.QueryCount(ctx, rule.SQL, start, now)
	if err != nil {
		return nil, err
	}
	return rule.hit(cnt, now, nil), nil
}

// EvaluateMem 是内存路径：对 entries 用 Rule.Eval 统计（测试 / MemStorage 模式）。
// 无需 Backend，窗口过滤在此完成。
func EvaluateMem(rule Rule, entries []*logstore.Entry, now time.Time) (*RuleHit, error) {
	d, err := ParseWindow(rule.Window)
	if err != nil {
		return nil, err
	}
	in := windowEntries(entries, now, d)
	cnt, sample := rule.Eval(in, now)
	return rule.hit(cnt, now, sample), nil
}

// CHBackend 是生产后端：经 clickhouse-go 连接参数化执行规则 SQL。
// 注意：参数顺序为 (窗口起止 time.Time, ...) —— clickhouse-go 原生支持 time.Time 绑定。
type CHBackend struct {
	conn driver.Conn
}

// NewCHBackend 包裹一个已建立的 clickhouse 连接。
func NewCHBackend(conn driver.Conn) *CHBackend { return &CHBackend{conn: conn} }

// QueryCount 执行规则 SQL，窗口起止作为前两个参数绑定。
func (b *CHBackend) QueryCount(ctx context.Context, query string, args ...any) (float64, error) {
	var v float64
	if err := b.conn.QueryRow(ctx, query, args...).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
