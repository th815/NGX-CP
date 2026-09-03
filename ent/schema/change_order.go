// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package schema 定义发布引擎的 ent 实体。
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// ChangeOrder 是一次配置变更的"工单"，聚合多个节点的 DeployTask。
//
// 状态机（控制面侧持久化，T030 契约）：
//
//	draft ──提交──> pending_approval ──批准──> pending
//	                      └──拒绝──> rejected
//	                 pending ──开始──> running ──┬──> success
//	                                              ├──> failed ──> rolling_back ──┬──> rolled_back
//	                                              │                            └──> rollback_failed ★
//	                                              └──> partial_success
//	转 canceled 的入口：draft / pending_approval / pending / running / failed / partial_success / rollback_failed
//
// 状态转换必须走 Service.Transition 的乐观锁更新，不能靠内存标志位。
type ChangeOrder struct {
	ent.Schema
}

func (ChangeOrder) Fields() []ent.Field {
	return []ent.Field{
		field.String("title"),
		// T030 契约：变更类型
		field.Enum("type").
			Values("config", "cert_renew", "security_block", "lvs", "upgrade", "rollback").
			Default("config"),
		// T030 契约：触发来源
		field.Enum("source").
			Values("manual", "api", "schedule", "auto_renew").
			Default("manual"),
		// 状态机主体（11 态）
		field.Enum("status").
			Values(
				"draft",
				"pending_approval",
				"pending",
				"running",
				"success",
				"failed",
				"rolling_back",
				"rolled_back",
				"rollback_failed",
				"partial_success",
				"rejected",
				"canceled",
			).
			Default("draft"),
		// 目标节点 ID 列表（JSON 数组）
		field.JSON("target_nodes", []int{}).
			Optional().
			SchemaType(map[string]string{"postgres": "jsonb", "sqlite": "json"}),
		// 目标配置版本 ID 列表（JSON 数组）
		field.JSON("config_revision_ids", []int{}).
			Optional().
			SchemaType(map[string]string{"postgres": "jsonb", "sqlite": "json"}),
		// 执行策略（嵌套结构，JSON）
		field.JSON("strategy", DeployStrategy{}).
			Optional().
			SchemaType(map[string]string{"postgres": "jsonb", "sqlite": "json"}),
		// 关联的发布前快照 ID（用于回滚）
		field.Int("snapshot_id").Optional().Nillable(),
		// 发起人（来自鉴权上下文 / 提交体）
		field.String("created_by").Optional(),
		// 审批人（批准时写入；空表示尚未审批）
		field.String("approved_by").Optional(),
		// 变更备注
		field.Text("comment").Optional(),
		// 执行开始 / 结束时间（T030 契约的时间戳）
		field.Time("started_at").Optional(),
		field.Time("finished_at").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional(),
	}
}

func (ChangeOrder) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("deploy_tasks", DeployTask.Type),
	}
}
