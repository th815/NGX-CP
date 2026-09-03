package probe

import (
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
)

// TestCatalogComplete 验证规则目录覆盖了磁盘/证书/安全/日志四类关键检查，且 critical 项非空。
func TestCatalogComplete(t *testing.T) {
	if len(Catalog) != 6 {
		t.Fatalf("Catalog 规模 = %d, want 6", len(Catalog))
	}
	cats := map[string]bool{}
	var critical int
	for _, r := range Catalog {
		cats[r.Category] = true
		if r.Severity == SeverityCritical {
			critical++
		}
		switch r.Severity {
		case SeverityCritical, SeverityWarning:
		default:
			t.Errorf("规则 %s 严重级别非法: %q", r.Name, r.Severity)
		}
	}
	for _, want := range []string{CatDisk, CatCert, CatSecurity, CatLog} {
		if !cats[want] {
			t.Errorf("Catalog 缺少分类 %q", want)
		}
	}
	if critical == 0 {
		t.Error("Catalog 至少应含 1 条 critical 规则")
	}
	// CatalogByName 索引正确。
	if len(CatalogByName()) != len(Catalog) {
		t.Error("CatalogByName 条目数不匹配")
	}
}

// TestEvaluateCriticalFail 验证任一 critical 项未通过即判定不健康。
func TestEvaluateCriticalFail(t *testing.T) {
	items := []*agentv1.ComplianceItem{
		{Name: "disk_usage_nginx_paths", Severity: SeverityCritical, Passed: false, Actual: "92%"},
		{Name: "log_dir_writable", Severity: SeverityWarning, Passed: true},
	}
	r := Evaluate(items)
	if r.Passed {
		t.Error("存在未通过 critical 项时 Passed 应为 false")
	}
	if len(r.CriticalFailed) != 1 || r.CriticalFailed[0] != "disk_usage_nginx_paths" {
		t.Errorf("CriticalFailed = %v, want [disk_usage_nginx_paths]", r.CriticalFailed)
	}
}

// TestEvaluateWarningNotBlocking 验证 warning 项不通过不触发 degraded（只 critical 才阻断）。
func TestEvaluateWarningNotBlocking(t *testing.T) {
	items := []*agentv1.ComplianceItem{
		{Name: "log_dir_writable", Severity: SeverityWarning, Passed: false},
		{Name: "pid_file_present", Severity: SeverityWarning, Passed: false},
		{Name: "disk_usage_nginx_paths", Severity: SeverityCritical, Passed: true},
	}
	if !Evaluate(items).Passed {
		t.Error("仅 warning 项失败不应判定不健康")
	}
}

// TestEvaluateEmpty 验证空/ nil 输入视为健康（调用方应在拿到真实报告后才驱动流转）。
func TestEvaluateEmpty(t *testing.T) {
	if !Evaluate(nil).Passed {
		t.Error("nil 输入应视为 Passed")
	}
	if !Evaluate([]*agentv1.ComplianceItem{}).Passed {
		t.Error("空切片应视为 Passed")
	}
}
