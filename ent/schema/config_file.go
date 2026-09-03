// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ConfigFile 表示某节点上一个"被管理的配置路径"的当前态指针。
// (node_id, path) 唯一：同一节点上同一路径只对应一个逻辑文件；
// 不同节点允许同名路径（这正是双机一致性比对的前提）。
//
// 版本链（ConfigRevision）通过自身的 node_id + path 字段与文件关联，
// 而非经 config_file 边——这样同一文件可挂任意多个版本，并能独立用
// current_revision_id 字段标记"当前生效版本"（避免与唯一边冲突）。
type ConfigFile struct {
	ent.Schema
}

func (ConfigFile) Fields() []ent.Field {
	return []ent.Field{
		field.Int("node_id"),
		field.String("path"),
		field.Enum("format").
			Values("nginx", "keepalived", "stream", "other").
			Default("nginx"),
		field.Int("current_revision_id").Optional(), // 当前生效版本 FK（指向 config_revisions.id），由发布链路维护
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional(),
	}
}

// Indexes (node_id, path) 复合唯一：同一节点同路径唯一，跨节点允许重名。
func (ConfigFile) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("node_id", "path").Unique(),
	}
}

// Edges 当前为空：版本链经 ConfigRevision 的 node_id+path 字段分组，
// 当前版本经 current_revision_id 字段标记，二者均不依赖边。
func (ConfigFile) Edges() []ent.Edge {
	return nil
}
