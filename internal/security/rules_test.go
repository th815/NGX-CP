package security

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/th/ngxcp/internal/logstore"
)

// mkEntry 构造一条测试日志。ts 默认放在 now（窗口内）。
func mkEntry(now time.Time, remote, uri string, status uint16, rt float32, b uint32, ua string) *logstore.Entry {
	return &logstore.Entry{
		TS: now, RemoteAddr: remote, URI: uri, Status: status,
		RequestRT: rt, Bytes: b, UA: ua, Node: "n1", RID: "r1",
	}
}

func TestDefaultRules_Structure(t *testing.T) {
	rules := DefaultRules()
	if len(rules) != 10 {
		t.Fatalf("期望 10 条规则，实际 %d", len(rules))
	}
	ids := make(map[string]bool)
	for _, r := range rules {
		if r.ID == "" || r.Name == "" {
			t.Errorf("规则 %s 缺 ID/Name", r.ID)
		}
		if ids[r.ID] {
			t.Errorf("规则 ID 重复: %s", r.ID)
		}
		ids[r.ID] = true

		switch r.Level {
		case LevelInfo, LevelWarn, LevelCritical:
		default:
			t.Errorf("%s 非法 Level: %q", r.ID, r.Level)
		}
		switch r.Action {
		case ActionAuto, ActionSemi, ActionAlert:
		default:
			t.Errorf("%s 非法 Action: %q", r.ID, r.Action)
		}
		if _, err := ParseWindow(r.Window); err != nil {
			t.Errorf("%s 非法 Window %q: %v", r.ID, r.Window, err)
		}
		if r.Threshold <= 0 {
			t.Errorf("%s Threshold 必须 > 0，实际 %v", r.ID, r.Threshold)
		}
		if r.SQL == "" {
			t.Errorf("%s SQL 为空", r.ID)
		}
		if r.Eval == nil {
			t.Errorf("%s Eval 为 nil", r.ID)
		}
	}
}

func TestParseWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"30s": 30 * time.Second,
		"5m":  5 * time.Minute,
		"1m":  time.Minute,
		"1h":  time.Hour,
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
	}
	for in, want := range cases {
		got, err := ParseWindow(in)
		if err != nil {
			t.Fatalf("ParseWindow(%q) 报错: %v", in, err)
		}
		if got != want {
			t.Errorf("ParseWindow(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseWindow("0"); err == nil {
		t.Error("ParseWindow(\"0\") 应失败")
	}
	if _, err := ParseWindow("bad"); err == nil {
		t.Error("ParseWindow(\"bad\") 应失败")
	}
}

func TestSQLInjection_TriggersCRITICAL(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	rule := mustRule(t, "r-sql-injection")
	es := []*logstore.Entry{
		mkEntry(now, "1.2.3.4", "/api/user?id=1 UNION SELECT password FROM users", 200, 0.1, 500, "Mozilla"),
		mkEntry(now, "1.2.3.4", "/login", 200, 0.1, 500, "Mozilla"),
	}
	hit, err := EvaluateMem(rule, es, now)
	if err != nil {
		t.Fatal(err)
	}
	if !hit.Triggered {
		t.Errorf("SQL 注入样本应触发，count=%v threshold=%v", hit.Count, hit.Threshold)
	}
	if hit.Level != LevelCritical {
		t.Errorf("SQL 注入应为 CRITICAL，实际 %q", hit.Level)
	}
	if hit.Count < 1 {
		t.Errorf("count 应 >= 1，实际 %v", hit.Count)
	}
	if hit.Sample == nil {
		t.Error("应带回证据样本")
	}
}

func TestCCFlood_Auto(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	rule := mustRule(t, "r-cc-flood")
	var es []*logstore.Entry
	for i := 0; i < 700; i++ {
		es = append(es, mkEntry(now, "9.9.9.9", "/", 200, 0.05, 300, "curl"))
	}
	hit, err := EvaluateMem(rule, es, now)
	if err != nil {
		t.Fatal(err)
	}
	if !hit.Triggered {
		t.Errorf("CC 洪水应触发，count=%v", hit.Count)
	}
	if hit.Action != ActionAuto {
		t.Errorf("CC 洪水应默认 auto，实际 %q", hit.Action)
	}

	// 正常流量（70 条）不应触发
	var normal []*logstore.Entry
	for i := 0; i < 70; i++ {
		normal = append(normal, mkEntry(now, "9.9.9.9", "/", 200, 0.05, 300, "Mozilla"))
	}
	hit2, _ := EvaluateMem(rule, normal, now)
	if hit2.Triggered {
		t.Errorf("70 条请求不应触发 CC 洪水，count=%v", hit2.Count)
	}
}

func TestDirBrute_Triggers(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	rule := mustRule(t, "r-dir-brute")
	var es []*logstore.Entry
	for i := 0; i < 50; i++ {
		es = append(es, mkEntry(now, "5.5.5.5", "/admin/page-"+itoa(i), 404, 0.02, 200, "gobuster"))
	}
	hit, err := EvaluateMem(rule, es, now)
	if err != nil {
		t.Fatal(err)
	}
	if !hit.Triggered {
		t.Errorf("目录爆破应触发，distinct 404 路径数=%v", hit.Count)
	}

	// 50 个 404 但分散在 50 个不同 IP（非爆破）
	var spread []*logstore.Entry
	for i := 0; i < 50; i++ {
		spread = append(spread, mkEntry(now, "1.1.1."+itoa(i), "/x-"+itoa(i), 404, 0.02, 200, "Mozilla"))
	}
	hit2, _ := EvaluateMem(rule, spread, now)
	if hit2.Triggered {
		t.Errorf("分散 IP 的 404 不应触发爆破，max=%v", hit2.Count)
	}
}

func TestSingleIPBroad_Triggers(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	rule := mustRule(t, "r-single-ip-broad")
	var es []*logstore.Entry
	for i := 0; i < 220; i++ {
		es = append(es, mkEntry(now, "7.7.7.7", "/p/"+itoa(i), 200, 0.1, 400, "Mozilla"))
	}
	hit, err := EvaluateMem(rule, es, now)
	if err != nil {
		t.Fatal(err)
	}
	if !hit.Triggered {
		t.Errorf("单 IP 遍历应触发，distinct uri=%v", hit.Count)
	}
}

func TestFalseNegative_NormalTraffic(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	// 正常流量：少量 200，分散 IP，正常 UA，无注入/敏感路径
	var es []*logstore.Entry
	for i := 0; i < 20; i++ {
		es = append(es, mkEntry(now, "10.0.0."+itoa(i), "/home", 200, 0.1, 1000, "Mozilla/5.0"))
	}
	rules := DefaultRules()
	for _, r := range rules {
		hit, err := EvaluateMem(r, es, now)
		if err != nil {
			t.Fatal(err)
		}
		if hit.Triggered && (r.Level == LevelCritical || r.Action == ActionAuto) {
			t.Errorf("正常流量不应触发 CRITICAL/auto 规则 %s（count=%v）", r.ID, hit.Count)
		}
	}
}

func TestEngine_EvaluateViaBackend(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	rule := mustRule(t, "r-cc-flood")

	e := NewEngine(&FakeBackend{Count: 999})
	hit, err := e.Evaluate(context.Background(), rule, now)
	if err != nil {
		t.Fatal(err)
	}
	if !hit.Triggered {
		t.Error("后端返回 999 应触发（阈值 600）")
	}

	e2 := NewEngine(&FakeBackend{Count: 1})
	hit2, _ := e2.Evaluate(context.Background(), rule, now)
	if hit2.Triggered {
		t.Error("后端返回 1 不应触发（阈值 600）")
	}

	// 后端报错应上抛
	e3 := NewEngine(&FakeBackend{Err: context.Canceled})
	if _, err := e3.Evaluate(context.Background(), rule, now); err == nil {
		t.Error("后端报错应上抛")
	}
}

// TestRules_SQLUsesParameters 断言每条规则 SQL 用 ? 占位窗口（参数化防注入）。
func TestRules_SQLUsesParameters(t *testing.T) {
	for _, r := range DefaultRules() {
		if n := strings.Count(r.SQL, "?"); n < 2 {
			t.Errorf("%s SQL 应至少含 2 个 ? 占位（窗口起止），实际 %d", r.ID, n)
		}
		if strings.Contains(r.SQL, "now()") || strings.Contains(r.SQL, "'"+r.Window+"'") {
			t.Errorf("%s SQL 不应拼接时间/窗口字面量（防注入）", r.ID)
		}
	}
}

// TestRules_EvalMatchesSQLIntent 用表面特征断言 Eval 与 SQL 未漂移：
// 每条规则的 SQL 必须包含其检测维度的关键标记。
func TestRules_EvalMatchesSQLIntent(t *testing.T) {
	want := map[string][]string{
		"r-sql-injection":   {"union select", "positionCaseInsensitive"},
		"r-scanner-ua":      {"sqlmap"},
		"r-dir-brute":       {"status = 404", "count(distinct uri)"},
		"r-cc-flood":        {"remote_addr", "count()"},
		"r-slowloris":       {"request_rt > 10", "bytes < 1024"},
		"r-5xx-spike":       {"status >= 500"},
		"r-4xx-spike":       {"status >= 400"},
		"r-sensitive-path":  {"/wp-admin"},
		"r-odd-ua":          {"ua = '-'"},
		"r-single-ip-broad": {"count(distinct uri)"},
	}
	for _, r := range DefaultRules() {
		subs, ok := want[r.ID]
		if !ok {
			t.Errorf("未定义 %s 的 SQL 意图断言", r.ID)
			continue
		}
		lower := strings.ToLower(r.SQL)
		for _, s := range subs {
			if !strings.Contains(lower, strings.ToLower(s)) {
				t.Errorf("%s SQL 缺少关键标记 %q（Eval 与 SQL 可能漂移）", r.ID, s)
			}
		}
	}
}

// ---------- 测试 helpers ----------

func mustRule(t *testing.T, id string) Rule {
	t.Helper()
	for _, r := range DefaultRules() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("未找到规则 %s", id)
	return Rule{}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
