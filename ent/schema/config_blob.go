package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// ConfigBlob 内容寻址配置块：相同内容只存一份（去重）。
// 通过 sha256 唯一约束实现去重；revision 通过 blob 边引用它。
type ConfigBlob struct {
	ent.Schema
}

func (ConfigBlob) Fields() []ent.Field {
	return []ent.Field{
		field.String("sha256").Unique(), // 内容 SHA256
		field.Int("size").Default(0),
		field.Text("content"), // 实际配置内容（大文本）
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (ConfigBlob) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("revisions", ConfigRevision.Type),
	}
}
