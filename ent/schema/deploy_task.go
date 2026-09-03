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

// DeployTask 是某个 ChangeOrder 在单个节点上的发布任务。
//
// 状态（state，8 态）与 ChangeOrder 状态机配合：
// 单节点任务也允许进入 rolling_back / rolled_back / rollback_failed，
// 以便回滚进度按节点精细追踪（T034）。
//
// node / change_order 通过边关联，FK 列由 ent 自动命名为 node_id / change_order_id。
type DeployTask struct {
	ent.Schema
}

func (DeployTask) Fields() []ent.Field {
	return []ent.Field{
		field.Enum("state").
			Values(
				"pending",
				"running",
				"success",
				"failed",
				"rolling_back",
				"rolled_back",
				"rollback_failed",
				"skipped",
			).
			Default("pending"),
		// 当前所处阶段：prepare → validate → canary → observe → rollback
		field.Enum("phase").
			Values("prepare", "validate", "canary", "observe", "rollback").
			Optional(),
		field.Int("attempts").Default(0),
		// 失败 / 回滚时的命令输出与错误信息（含 nginx -t 输出）
		field.Text("error_detail").Optional(),
		// T030 契约：9 步执行序列（传输/校验/快照/切换/reload/探活/...）
		field.JSON("steps", []TaskStep{}).
			Optional().
			SchemaType(map[string]string{"postgres": "jsonb", "sqlite": "json"}),
		// 当前所处步骤索引
		field.Int("current_step").Default(0),
		field.Time("started_at").Optional(),
		field.Time("finished_at").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional(),
	}
}

func (DeployTask) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("node", Node.Type).
			Ref("deploy_tasks").
			Unique(),
		edge.From("change_order", ChangeOrder.Type).
			Ref("deploy_tasks").
			Unique(), // 一个发布任务只属于一个变更单
	}
}
