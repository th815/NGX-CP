// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ConfigVariable 是三级变量的一行：global（全平台）/ cluster（某集群）/ node（单节点）。
// 同 (scope, target_id, key) 唯一；渲染时按 global < cluster < node 优先级覆盖合并。
type ConfigVariable struct {
	ent.Schema
}

func (ConfigVariable) Fields() []ent.Field {
	return []ent.Field{
		field.Enum("scope").
			Values("global", "cluster", "node").
			Comment("变量作用域：global 全平台 / cluster 某集群 / node 单节点"),
		// target_id：cluster 作用域存 cluster_id；node 作用域存 node_id；global 作用域恒为 0。
		field.Int("target_id").Default(0).Comment("cluster_id 或 node_id；global 时为 0"),
		field.String("key").Comment("变量键"),
		field.String("value").Comment("变量值（secret 为真时仅内部渲染使用，API 返回打码）"),
		field.Bool("secret").Default(false).Comment("敏感值，API 返回时打码"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

// Indexes (scope, target_id, key) 复合唯一：同一作用域同一目标同一键只有一行。
func (ConfigVariable) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("scope", "target_id", "key").Unique(),
	}
}

func (ConfigVariable) Edges() []ent.Edge {
	return nil
}
