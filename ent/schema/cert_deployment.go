// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package schema 定义证书子系统的 ent 实体（M4）。
package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// CertDeployment 一张证书在某个节点上的分发状态（T044 写入）。
//
// 状态流转：pending → deployed | failed。
type CertDeployment struct {
	ent.Schema
}

// Fields 分发记录字段（certificate_id 由下方 edge 自动生成）。
func (CertDeployment) Fields() []ent.Field {
	return []ent.Field{
		field.Int("node_id").
			Comment("目标节点 ID"),
		field.Enum("status").
			Values("pending", "deployed", "failed").
			Default("pending").
			Comment("分发状态"),
		field.Time("deployed_at").Optional().
			Comment("实际落盘时间（UTC）"),
		field.String("error").Optional().
			Comment("失败原因（人类可读）"),
	}
}

// Edges 每条分发记录隶属一张证书（多对一）。
func (CertDeployment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("certificate", Certificate.Type).
			Ref("deployments").
			Unique().
			Required(),
	}
}
