// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package schema 定义发布引擎的 ent 实体。
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Approval 记录一次变更单的审批决策（审计轨迹）。
//
// 一个变更单同一时刻只有一条待审记录（order_id 唯一）。
// 状态流转：pending → approved | rejected | expired。
// expired 由控制面定时 worker（Service.ExpireApprovals）在超过 expires_at 后写入。
type Approval struct {
	ent.Schema
}

func (Approval) Fields() []ent.Field {
	return []ent.Field{
		field.Int("order_id").Unique(),
		// 触发审批的规则名（来自 ApprovalRule.Name）；空表示显式声明或兜底规则。
		field.String("required_by").Optional(),
		field.Enum("status").
			Values("pending", "approved", "rejected", "expired").
			Default("pending"),
		// 审批人（与 ChangeOrder.created_by / approved_by 同为用户名字符串，保持一致性）。
		field.String("approver").Optional(),
		field.String("comment").Optional(),
		// 决策时间（批准/拒绝/过期时写入）。
		field.Time("decided_at").Optional(),
		// 超时时间；为空表示永不过期（运维可依赖此字段做 SLA 看板）。
		field.Time("expires_at").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Approval) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status"),
	}
}
