// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package lvs 的发布前门禁（M5 T055）。
//
// 门禁语义：DR 合规 / 节点状态任一项不通过 → 阻断该节点参与 LVS 发布（调权 / 配置下发）。
// 阻断的是"发布"，不是业务——节点仍正常承载流量。前端亦会据此禁用发布按钮（纵深防御）。
package lvs

import (
	"context"
	"strings"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// ComplianceChecker 判定某节点是否允许参与 LVS 发布。
// 返回 passed=false 时附带未通过的关键项名（驱动阻断原因与前端提示）。
type ComplianceChecker func(ctx context.Context, nodeID int) (passed bool, failed []string, err error)

// Gate 发布前门禁（T055）。
type Gate struct {
	check ComplianceChecker
}

// NewGate 构造门禁。check 为 nil 时放行（仅查询 / 无 Agent 通道场景）。
func NewGate(check ComplianceChecker) *Gate { return &Gate{check: check} }

// Check 判定 nodeID 是否可参与发布。err 表示判定本身失败（如存储异常），
// 与"不通过"语义不同：判定失败按不可用处理，避免错误放行。
func (g *Gate) Check(ctx context.Context, nodeID int) error {
	if g.check == nil {
		return nil
	}
	passed, failed, err := g.check(ctx, nodeID)
	if err != nil {
		return apperr.Wrap(apperr.CodeUnavailable, "合规门禁判定失败", err)
	}
	if !passed {
		return apperr.New(apperr.CodePrecondition,
			"节点合规未通过，阻断 LVS 发布："+strings.Join(failed, "、"))
	}
	return nil
}

// NodeStatusCompliance 基于节点状态（degraded / offline / decommissioned）判定是否可发布。
// 完整 DR 合规（6 项）落库后应在 check 中纳入 Agent 合规上报；当前以节点状态作代理。
func NodeStatusCompliance(client *ent.Client) ComplianceChecker {
	return func(ctx context.Context, nodeID int) (bool, []string, error) {
		n, err := client.Node.Get(ctx, nodeID)
		if err != nil {
			if ent.IsNotFound(err) {
				return false, []string{"node_not_found"}, nil
			}
			return false, nil, err
		}
		switch n.Status {
		case "degraded", "offline", "decommissioned":
			return false, []string{"node_status_" + string(n.Status)}, nil
		}
		return true, nil, nil
	}
}
