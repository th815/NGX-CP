package compliance

import (
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
)

func TestCatalog(t *testing.T) {
	if len(Catalog) == 0 {
		t.Fatal("合规规则目录为空")
	}
	byName := CatalogByName()
	// 关键 DR 规则必须存在。
	for _, must := range []string{"vip_on_lo", "arp_suppress", "keepalived_unicast", "no_ah_auth", "director_promisc"} {
		if _, ok := byName[must]; !ok {
			t.Errorf("缺少关键合规规则: %s", must)
		}
	}
}

// TestEvaluateCriticalFail 验证：任一 critical 项不通过 → 整体不合规，并列出失败项。
func TestEvaluateCriticalFail(t *testing.T) {
	report := &agentv1.ComplianceReport{
		CheckedAt: 123,
		Items: []*agentv1.ComplianceItem{
			{Name: "vip_on_lo", Severity: SeverityCritical, Passed: true},
			{Name: "arp_suppress", Severity: SeverityCritical, Passed: false, Actual: "arp_ignore=0"},
			{Name: "time_sync", Severity: SeverityWarning, Passed: false}, // warning 失败不影响整体
		},
	}
	r := Evaluate(report)
	if r.Passed {
		t.Error("存在未通过的 critical 项却判定为合规")
	}
	if len(r.CriticalFailed) != 1 || r.CriticalFailed[0] != "arp_suppress" {
		t.Errorf("CriticalFailed = %v, want [arp_suppress]", r.CriticalFailed)
	}
}

// TestEvaluateClean 验证：所有 critical 项通过（warning 可失败）→ 整体合规。
func TestEvaluateClean(t *testing.T) {
	report := &agentv1.ComplianceReport{
		Items: []*agentv1.ComplianceItem{
			{Name: "vip_on_lo", Severity: SeverityCritical, Passed: true},
			{Name: "arp_suppress", Severity: SeverityCritical, Passed: true},
			{Name: "time_sync", Severity: SeverityWarning, Passed: false},
		},
	}
	r := Evaluate(report)
	if !r.Passed {
		t.Error("warning 失败不应导致整体不合规")
	}
}

// TestEvaluateNil 验证：nil 报告返回零值（不驱动状态流转）。
func TestEvaluateNil(t *testing.T) {
	r := Evaluate(nil)
	if !r.Passed || len(r.CriticalFailed) != 0 {
		t.Errorf("nil 报告应返回零值, got %+v", r)
	}
}
