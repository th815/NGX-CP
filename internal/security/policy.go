// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package security 的分级处置策略（T069）。
//
// T066 给每条规则配了 Action（auto / semi / alert）。T067 的调度器命中规则后只建事件，
// 本文件把「命中后怎么处置」从调度器里抽出来独立编排，便于单测与演进：
//   - alert: 只记录事件，等人工在 UI 处置（不封禁）
//   - semi : 创建封禁变更单并进入「待审批」状态机（requireApproval=true），人工批准才下发
//   - auto : 高置信直接封禁（requireApproval=false）；仅对误伤面极小的规则开放（如 r-cc-flood）
//
// 任何 auto 动作都有回滚路径：T068 的 security_block 变更单自带 lvs_graceful 灰度 +
// AutoRollback:true，且调度器去重保证同一持续攻击只处置一次，避免重复封禁。
package security

import (
	"context"
	"fmt"

	"github.com/th/ngxcp/ent"
)

// BlockExecutor 是分级处置策略对「封禁执行」的抽象（T069）。
// 用接口而非直接 *BlockService，便于单测用 mock 验证动作分流，且不依赖数据库。
type BlockExecutor interface {
	// BlockIP 封禁一个 IP。requireApproval=true 进入待审批状态机（semi 动作）。
	BlockIP(ctx context.Context, ip, reason, operator string, requireApproval bool) (*ent.ChangeOrder, error)
}

// ApplyPolicy 按规则动作分级处置一条命中的安全事件（T069 核心契约）。
//
// 返回 applied 为实际执行的动作（ActionAuto / ActionSemi / ActionAlert），err 非 nil 表示
// 处置失败（如 auto/semi 但样本里提取不到合法 IP 时，绝不拿空地址去封禁）。
//
// evt.Sample 中提取来源 IP；operator 标记自动处置来源（如 "system(policy)"），
// 便于审计区分「自动策略」与「人工点封禁」。
func ApplyPolicy(ctx context.Context, rule Rule, evt Event, block BlockExecutor, operator string) (applied string, err error) {
	switch rule.Action {
	case ActionAlert:
		// alert 只留事件，等人工在 UI 里处置；不自动封禁，避免误伤。
		return ActionAlert, nil
	case ActionSemi, ActionAuto:
		ip, ok := ExtractIP(evt.Sample)
		if !ok {
			return "", fmt.Errorf("规则 %s 命中但样本中无合法 IP，放弃 %s 封禁（不误封空地址）",
				rule.ID, actionLabel(rule.Action))
		}
		// semi 走审批、auto 直接执行；两者都经 T068 变更单（含自动回滚兜底）。
		requireApproval := rule.Action == ActionSemi
		if _, berr := block.BlockIP(ctx, ip, policyReason(rule), operator, requireApproval); berr != nil {
			return "", fmt.Errorf("规则 %s %s 封禁失败: %w", rule.ID, actionLabel(rule.Action), berr)
		}
		return rule.Action, nil
	default:
		return "", fmt.Errorf("规则 %s 未知动作 %q", rule.ID, rule.Action)
	}
}

// actionLabel 把动作常量转成可读标签（用于日志/审计）。
func actionLabel(a string) string {
	switch a {
	case ActionAuto:
		return "auto"
	case ActionSemi:
		return "semi"
	case ActionAlert:
		return "alert"
	default:
		return a
	}
}

// policyReason 生成自动处置的变更单 comment，标注来源动作与规则，便于事后审计。
func policyReason(rule Rule) string {
	return fmt.Sprintf("T069 策略自动处置（%s）：规则「%s」命中", actionLabel(rule.Action), rule.Name)
}
