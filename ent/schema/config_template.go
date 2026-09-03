// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// ConfigTemplate 表示一个 nginx 配置模板（Go template 语法）。
// 模板通过三级变量（global < cluster < node）渲染出不同节点各自的配置，
// 是"一处改全局达"的载体（AGENTS.md §0 支柱一）。
type ConfigTemplate struct {
	ent.Schema
}

func (ConfigTemplate) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").Unique().Comment("模板名，如 upstream"),
		field.Text("content").Comment("Go template 语法内容"),
		field.String("applies_to").Comment("目标路径模式，如 conf.d/upstream-{cluster}.conf"),
		// variables 是模板引用的变量清单（渲染前自动提取，便于 UI 展示与缺失校验）。
		field.JSON("variables", []string{}).Optional().Comment("模板引用的变量清单（自动提取）"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional(), // 软删除
	}
}

func (ConfigTemplate) Edges() []ent.Edge {
	return nil
}
