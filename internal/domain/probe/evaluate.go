package probe

import agentv1 "github.com/th/ngxcp/gen/agent/v1"

// Result 是 FS/日志健康探测的判定结果。
type Result struct {
	// Passed 表示整体是否健康：任一 critical 项不通过即 false。
	Passed bool
	// CriticalFailed 列出未通过的关键项名称（驱动 degraded 的依据）。
	CriticalFailed []string
	// Items 是 Agent 上报的原始条目（透传，便于前端逐项展示）。
	Items []*agentv1.ComplianceItem
}

// Evaluate 聚合 Agent 上报的 FS/日志探测结果：只要存在未通过的 critical 项即判定不健康。
// 传入 nil 或空切片时返回 Passed=true（调用方应在拿到真实报告后才驱动状态流转）。
func Evaluate(items []*agentv1.ComplianceItem) Result {
	res := Result{Items: items}
	for _, it := range items {
		if !it.GetPassed() && it.GetSeverity() == SeverityCritical {
			res.CriticalFailed = append(res.CriticalFailed, it.GetName())
		}
	}
	res.Passed = len(res.CriticalFailed) == 0
	return res
}
