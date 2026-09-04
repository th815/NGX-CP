// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 实现审批流（T036）：规则引擎 + 审批审计实体 + 自审批拦截 + 超时过期。
package deploy

import (
	"context"
	"time"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/approval"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// ApprovalRule 单条审批规则（结构化匹配，避免引入 CEL 表达式引擎依赖）。
// 命中条件（全部满足）：Enabled 且
//   - Types 为空 或 含 co.Type
//   - Sources 为空 或 含 co.Source
//   - MinNodes==0 或 len(co.TargetNodes) >= MinNodes
type ApprovalRule struct {
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Types    []string `json:"types"`     // 空 = 任意类型
	Sources  []string `json:"sources"`   // 空 = 任意来源
	MinNodes int      `json:"min_nodes"` // 0 = 不限制节点数
}

// ApprovalConfig 审批总开关与规则集，可由控制面配置注入覆盖默认值。
type ApprovalConfig struct {
	Enabled           bool           `json:"enabled"`
	AllowSelfApproval bool           `json:"allow_self_approval"`
	Timeout           time.Duration  `json:"timeout"` // 超时自动拒绝
	Rules             []ApprovalRule `json:"rules"`
}

// DefaultApprovalConfig 与任务契约一致的内置默认规则。
// 注：契约原文以字段表达式描述（如 cluster==” && node_count>=2、path=='nginx.conf'），
// 本项目以结构化字段（Types/Sources/MinNodes）等价表达，避免引入表达式引擎；
// 后续如需更复杂条件（路径/集群标签）可扩展结构体字段。
func DefaultApprovalConfig() *ApprovalConfig {
	return &ApprovalConfig{
		Enabled:           true,
		AllowSelfApproval: false,
		Timeout:           24 * time.Hour,
		Rules: []ApprovalRule{
			{Name: "生产集群全量变更", Enabled: true, MinNodes: 2},
			{Name: "LVS 配置变更", Enabled: true, Types: []string{"lvs"}},
			{Name: "二进制升级", Enabled: true, Types: []string{"upgrade"}},
			{Name: "证书续期(非自动)", Enabled: true, Types: []string{"cert_renew"}},
		},
	}
}

// SetApprovalConfig 注入审批配置（不破坏既有 New(client) 调用方）；nil 时用默认。
func (s *Service) SetApprovalConfig(cfg *ApprovalConfig) { s.approvalCfg = cfg }

// cfg 返回生效中的审批配置（注入优先，否则内置默认）。
func (s *Service) cfg() *ApprovalConfig {
	if s.approvalCfg != nil {
		return s.approvalCfg
	}
	return DefaultApprovalConfig()
}

// RequiresApproval 判断该变更单是否需要审批，返回 bool 与命中的规则名。
func (cfg *ApprovalConfig) RequiresApproval(co *ent.ChangeOrder) (bool, string) {
	if cfg == nil || !cfg.Enabled {
		return false, ""
	}
	// 证书自动续期（auto_renew）绝不应触发人工审批。
	if string(co.Source) == "auto_renew" {
		return false, ""
	}
	// 变更单显式声明需审批（DeployStrategy.ApprovalRequired）。
	if co.Strategy.ApprovalRequired {
		return true, "strategy.approval_required"
	}
	for _, r := range cfg.Rules {
		if ruleMatches(r, co) {
			return true, r.Name
		}
	}
	return false, ""
}

// ruleMatches 结构化规则匹配。
func ruleMatches(r ApprovalRule, co *ent.ChangeOrder) bool {
	if !r.Enabled {
		return false
	}
	if len(r.Types) > 0 && !strSliceContains(r.Types, string(co.Type)) {
		return false
	}
	if len(r.Sources) > 0 && !strSliceContains(r.Sources, string(co.Source)) {
		return false
	}
	if r.MinNodes > 0 && len(co.TargetNodes) < r.MinNodes {
		return false
	}
	return true
}

// strSliceContains 是切片包含判断（小工具）。
func strSliceContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// EvaluateApproval 供 handler 在提交前预判是否需要审批（用于响应字段）。
func (s *Service) EvaluateApproval(ctx context.Context, orderID int) (need bool, rule string, err error) {
	co, err := s.Get(ctx, orderID)
	if err != nil {
		return false, "", err
	}
	need, rule = s.cfg().RequiresApproval(co)
	return need, rule, nil
}

// createApproval 落一条 pending 状态的审批记录（含超时时间）。
func (s *Service) createApproval(ctx context.Context, orderID int, rule string) error {
	exp := time.Now().Add(s.cfg().Timeout)
	_, err := s.client.Approval.Create().
		SetOrderID(orderID).
		SetRequiredBy(rule).
		SetExpiresAt(exp).
		Save(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "创建审批记录失败", err)
	}
	return nil
}

// markApproval 将 pending 审批记录置为批准/拒绝，并写入决策人与时间。
// 无 pending 记录（已决/已过期）时返回冲突错误。
func (s *Service) markApproval(ctx context.Context, orderID int, status, approver, comment string) error {
	upd := s.client.Approval.Update().
		Where(approval.OrderIDEQ(orderID), approval.StatusEQ(approval.StatusPending))
	switch status {
	case "approved":
		upd = upd.SetStatus(approval.StatusApproved).SetApprover(approver).SetDecidedAt(time.Now())
	case "rejected":
		upd = upd.SetStatus(approval.StatusRejected).SetApprover(approver).SetComment(comment).SetDecidedAt(time.Now())
	default:
		return apperr.New(apperr.CodeInvalid, "非法的审批动作："+status)
	}
	n, err := upd.Save(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "更新审批记录失败", err)
	}
	if n == 0 {
		return apperr.New(apperr.CodeConflict, "该变更单没有待审批记录（可能已审批或已过期）")
	}
	return nil
}

// ExpireApprovals 将超时未决的审批标记为 expired，并把对应变更单置为 rejected。
// 由控制面定时 worker 调用，返回过期条数。
func (s *Service) ExpireApprovals(ctx context.Context) (int, error) {
	now := time.Now()
	due, err := s.client.Approval.Query().
		Where(approval.StatusEQ(approval.StatusPending), approval.ExpiresAtLT(now)).
		All(ctx)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "查询超时审批失败", err)
	}
	expired := 0
	for _, a := range due {
		if _, err := a.Update().SetStatus(approval.StatusExpired).SetDecidedAt(now).Save(ctx); err != nil {
			return expired, apperr.Wrap(apperr.CodeInternal, "标记审批过期失败", err)
		}
		// 变更单可能已被并发处理（极端情况），冲突/非法时忽略。
		if err := s.Transition(ctx, a.OrderID, string(StatusPendingApproval), string(StatusRejected)); err != nil {
			if apperr.CodeOf(err) == apperr.CodeConflict || apperr.CodeOf(err) == apperr.CodeInvalid {
				continue
			}
			return expired, err
		}
		expired++
	}
	return expired, nil
}

// ListApprovals 列出审批记录（?status= 可选过滤），按创建时间倒序。
func (s *Service) ListApprovals(ctx context.Context, statusFilter string) ([]*ent.Approval, error) {
	q := s.client.Approval.Query().Order(ent.Desc(approval.FieldCreatedAt))
	if statusFilter != "" {
		q = q.Where(approval.StatusEQ(approval.Status(statusFilter)))
	}
	items, err := q.All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "列出审批失败", err)
	}
	return items, nil
}

// GetApproval 取某变更单的最新审批记录。
func (s *Service) GetApproval(ctx context.Context, orderID int) (*ent.Approval, error) {
	a, err := s.client.Approval.Query().
		Where(approval.OrderIDEQ(orderID)).
		Order(ent.Desc(approval.FieldCreatedAt)).
		First(ctx)
	if ent.IsNotFound(err) {
		return nil, apperr.New(apperr.CodeNotFound, "无审批记录")
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "读取审批失败", err)
	}
	return a, nil
}
