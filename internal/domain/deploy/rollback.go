// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 实现发布引擎的领域模型：变更单状态机与持久化转换（T030）。
//
// rollback.go（T034）负责把"回滚"上升为受控的领域操作：
//   - RollbackNode：回滚单个节点到指定快照，Agent 回滚失败即 rollback_failed，触发 CRITICAL 告警。
//   - RollbackChangeOrder：逆序回滚变更单的全部目标节点（最后变更的先回滚），
//     整体状态机 running/failed/partial_success → rolling_back → rolled_back | rollback_failed。
//
// 触发 Agent 执行与发告警都是可注入的接口（NodeRollbackClient / AlertSink），
// 控制面经 proto Heartbeat ROLLBACK_CONFIG 命令接线，单测用 fake 注入，领域层不耦合任何传输细节。
package deploy

import (
	"context"
	"fmt"
	"sort"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

// RollbackMode 选择回滚策略（T034）。Agent 侧只实现 snapshot 模式；
// revision 模式由控制面在配置版本链层完成，不经 Agent 回滚执行器。
type RollbackMode string

const (
	RollbackSnapshot RollbackMode = "snapshot" // 从文件快照恢复（默认，最可靠）
	RollbackRevision RollbackMode = "revision" // 从配置版本链恢复（更快，仅改配置）
)

// NodeRollbackClient 是触发某节点执行回滚的抽象。
// 真实实现通过 Heartbeat ROLLBACK_CONFIG 命令下发（见 proto），由 Agent 端 RollbackExecutor 执行。
type NodeRollbackClient interface {
	// RollbackNodeConfig 让 nodeID 从 snapshotPath 指定的快照回滚。
	// 返回的 restored 表示 Agent 是否真正恢复了文件（用于审计/告警）。
	RollbackNodeConfig(ctx context.Context, nodeID int, snapshotPath string) (restored bool, err error)
}

// AlertSink 是告警出口（T034）。rollback_failed 必触发 CRITICAL——节点处于未知状态，需立即人工介入。
type AlertSink interface {
	Critical(ctx context.Context, msg string, kv map[string]any) error
}

// SetRollbackClient 注入回滚触发客户端（可选；未注入时回滚类方法返回 CodeInternal）。
func (s *Service) SetRollbackClient(c NodeRollbackClient) { s.rbClient = c }

// SetAlertSink 注入告警出口（可选；未注入时静默跳过告警）。
func (s *Service) SetAlertSink(a AlertSink) { s.alert = a }

// RollbackNode 回滚单个节点到指定快照。
// 成功返回 nil；Agent 回滚失败（rollback_failed）时触发 CRITICAL 告警并返回错误。
func (s *Service) RollbackNode(ctx context.Context, nodeID int, snapshotPath string, mode RollbackMode) error {
	if s.rbClient == nil {
		return apperr.New(apperr.CodeInternal, "回滚客户端未注入")
	}
	if snapshotPath == "" {
		return apperr.New(apperr.CodeInvalid, "回滚缺少快照路径")
	}
	restored, err := s.rbClient.RollbackNodeConfig(ctx, nodeID, snapshotPath)
	if err != nil {
		s.emitCritical(ctx, "节点回滚失败（rollback_failed）", map[string]any{
			"node_id":  nodeID,
			"snapshot": snapshotPath,
			"mode":     string(mode),
			"restored": restored,
			"error":    err.Error(),
		})
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("节点 %d 回滚失败（rollback_failed）", nodeID), err)
	}
	return nil
}

// RollbackChangeOrder 逆序回滚变更单的全部目标节点（最后变更的先回滚）。
//
// 状态机：running/failed/partial_success → rolling_back →
//
//	（全部成功）rolled_back ｜（任一节点失败）rollback_failed + CRITICAL 告警 + 冻结变更。
//
// snapshots 为 nodeID→快照路径，需覆盖全部目标节点；缺快照视为回滚失败。
func (s *Service) RollbackChangeOrder(ctx context.Context, orderID int, snapshots map[int]string) error {
	if s.rbClient == nil {
		return apperr.New(apperr.CodeInternal, "回滚客户端未注入")
	}
	co, err := s.Get(ctx, orderID)
	if err != nil {
		return err
	}
	from := OrderStatus(string(co.Status))
	if !canRollbackFrom(from) {
		return apperr.New(apperr.CodeInvalid, fmt.Sprintf("变更单状态 %s 不允许回滚", co.Status))
	}

	// 进入 rolling_back（乐观锁：起始态为当前 co.Status，并发冲突返回 CodeConflict）。
	if err := s.Transition(ctx, orderID, string(from), string(StatusRollingBack)); err != nil {
		return err
	}

	// 逆序：最后变更的节点先回滚（契约要求）。
	nodes := append([]int(nil), co.TargetNodes...)
	sort.Sort(sort.Reverse(sort.IntSlice(nodes)))

	failedNode := -1
	for _, nid := range nodes {
		snap, ok := snapshots[nid]
		if !ok {
			failedNode = nid
			s.emitCritical(ctx, "回滚缺少节点快照路径", map[string]any{"order_id": orderID, "node_id": nid})
			break
		}
		if err := s.RollbackNode(ctx, nid, snap, RollbackSnapshot); err != nil {
			failedNode = nid
			break
		}
	}

	if failedNode >= 0 {
		// rollback_failed：节点处于未知状态，冻结变更，需人工介入。
		_ = s.Transition(ctx, orderID, string(StatusRollingBack), string(StatusRollbackFailed))
		s.emitCritical(ctx, "变更单回滚失败，已冻结后续变更", map[string]any{
			"order_id":    orderID,
			"failed_node": failedNode,
		})
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("变更单回滚在节点 %d 失败，已置 rollback_failed 并冻结", failedNode))
	}

	if err := s.Transition(ctx, orderID, string(StatusRollingBack), string(StatusRolledBack)); err != nil {
		return err
	}
	return nil
}

// canRollbackFrom 判断起始状态是否允许进入回滚流程。
func canRollbackFrom(from OrderStatus) bool {
	switch from {
	case StatusRunning, StatusFailed, StatusPartialSuccess:
		return true
	}
	return false
}

// emitCritical 经注入的告警出口发 CRITICAL（未注入则静默）。
func (s *Service) emitCritical(ctx context.Context, msg string, kv map[string]any) {
	if s.alert == nil {
		return
	}
	_ = s.alert.Critical(ctx, msg, kv)
}
