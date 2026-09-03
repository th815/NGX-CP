package compliance

import agentv1 "github.com/th/ngxcp/gen/agent/v1"

// Result 是合规判定结果。
type Result struct {
	// Passed 表示整体是否合规：任一 critical 项不通过即 false。
	Passed bool
	// CriticalFailed 列出未通过的关键项名称（驱动 degraded 的依据）。
	CriticalFailed []string
	// Items 是 Agent 上报的原始条目（透传，便于前端逐项展示）。
	Items []*agentv1.ComplianceItem
}

// Evaluate 聚合 Agent 上报的合规报告：只要存在未通过的 critical 项即判定不合规。
// report 为 nil 时视为"无失败关键项"返回 Passed=true（调用方应在拿到真实报告后才驱动状态流转，
// 故 nil 不会误把未知节点标为不合规）。
func Evaluate(report *agentv1.ComplianceReport) Result {
	res := Result{}
	if report == nil {
		res.Passed = true
		return res
	}
	res.Items = report.GetItems()
	for _, it := range report.GetItems() {
		if !it.GetPassed() && it.GetSeverity() == SeverityCritical {
			res.CriticalFailed = append(res.CriticalFailed, it.GetName())
		}
	}
	res.Passed = len(res.CriticalFailed) == 0
	return res
}
