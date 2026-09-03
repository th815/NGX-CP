// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package schema 定义发布引擎的持久化数据结构（DeployStrategy / TaskStep）。
//
// 这些结构被 ent field.JSON 直接序列化进 ChangeOrder / DeployTask 行，
// 因此放在 schema 包（叶子包），避免 internal/domain/deploy 反向依赖 ent/schema
// 造成的导入环（deploy → ent → ent/schema → deploy）。
package schema

import "time"

// DeployStrategy 描述一次变更单的执行策略（T030 契约）。
// 序列化为 ChangeOrder.strategy 列（JSON）。
type DeployStrategy struct {
	// Mode 取值：serial | batch | all_at_once | lvs_graceful。
	Mode string `json:"mode"`
	// BatchSize serial 模式下忽略；batch 模式每批节点数。
	BatchSize int `json:"batch_size"`
	// ObserveWindow 每批之后的观测时长（秒）。
	ObserveWindow int `json:"observe_window"`
	// FailureThreshold 失败率阈值，默认 0（任一失败即熔断）。
	FailureThreshold float64 `json:"failure_threshold"`
	// AutoRollback 失败后是否自动回滚，默认 true。
	AutoRollback bool `json:"auto_rollback"`
	// ApprovalRequired 是否需要人工审批。
	ApprovalRequired bool `json:"approval_required"`
}

// TaskStep 是单节点发布任务的某一步骤快照（T030 契约）。
// 序列化为 DeployTask.steps 列（JSON 数组）。
type TaskStep struct {
	Name string `json:"name"` // transfer | validate | snapshot | switch | reload | probe | rollback ...
	// Status 取值：pending | running | success | failed | skipped。
	Status string `json:"status"`
	// StartedAt / FinishedAt 使用 RFC3339 字符串，便于 JSON 存储与前端解析。
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Output     string `json:"output,omitempty"`
}

// StepTimings 把时间字段格式化为字符串，供 TaskStep 持久化使用。
func stepTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
