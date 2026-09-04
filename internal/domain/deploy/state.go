// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 实现发布引擎的领域模型：变更单状态机与持久化转换（T030）。
//
// 状态机是控制面侧的核心不变量：所有状态迁移都必须通过 Service.Transition 的
// 乐观锁写库完成，绝不能只放内存（控制面重启后要能恢复 running 的变更单）。
package deploy

// OrderStatus 是 ChangeOrder 状态机的状态，取值与 ent/schema 中 changeorder.Status 一一对应。
type OrderStatus string

const (
	StatusDraft           OrderStatus = "draft"
	StatusPendingApproval OrderStatus = "pending_approval"
	StatusPending         OrderStatus = "pending"
	StatusRunning         OrderStatus = "running"
	StatusSuccess         OrderStatus = "success"
	StatusFailed          OrderStatus = "failed"
	StatusRollingBack     OrderStatus = "rolling_back"
	StatusRolledBack      OrderStatus = "rolled_back"
	StatusRollbackFailed  OrderStatus = "rollback_failed"
	StatusPartialSuccess  OrderStatus = "partial_success"
	StatusRejected        OrderStatus = "rejected"
	StatusCanceled        OrderStatus = "canceled"
)

// transitions 定义合法的（from → to）迁移集合。
// 这是状态机的唯一事实来源，Service.Transition 据此做合法性校验。
var transitions = map[OrderStatus][]OrderStatus{
	StatusDraft:           {StatusPendingApproval, StatusPending, StatusCanceled},
	StatusPendingApproval: {StatusPending, StatusRejected, StatusCanceled},
	StatusPending:         {StatusRunning, StatusCanceled},
	StatusRunning:         {StatusSuccess, StatusFailed, StatusPartialSuccess, StatusRollingBack, StatusCanceled},
	StatusFailed:          {StatusRollingBack, StatusCanceled},
	StatusPartialSuccess:  {StatusRollingBack, StatusCanceled},
	StatusRollingBack:     {StatusRolledBack, StatusRollbackFailed},
	StatusRolledBack:      {StatusPending},  // 回滚完成后可重新提交
	StatusRollbackFailed:  {StatusCanceled}, // 节点处于未知状态，需人工介入后取消/重做
	// 终止态：无出边
	StatusSuccess:  {},
	StatusRejected: {},
	StatusCanceled: {},
}

// CanTransition 判断 from → to 是否为合法迁移。
func CanTransition(from, to OrderStatus) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AllowedTransitions 返回从 from 出发的全部合法目标状态（用于前端按钮可用性等）。
func AllowedTransitions(from OrderStatus) []OrderStatus {
	out := make([]OrderStatus, len(transitions[from]))
	copy(out, transitions[from])
	return out
}

// IsTerminal 判断状态是否为终止态（无合法出边）。
func IsTerminal(s OrderStatus) bool {
	return len(transitions[s]) == 0
}
