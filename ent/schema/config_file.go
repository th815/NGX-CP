package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// ConfigFile 表示某节点上一个"被管理的配置路径"的当前态指针，
// 通过 current_revision 边指向当前生效的 ConfigRevision。
// (node_id, path) 应唯一，但 SQLite/PG 跨字段唯一约束在 M0 用应用层保证，
// 后续可加 ent 唯一索引注解强化。
type ConfigFile struct {
	ent.Schema
}

func (ConfigFile) Fields() []ent.Field {
	return []ent.Field{
		field.Int("node_id"),
		field.String("path").Unique(), // 在单节点范围内 path 唯一；跨节点由 node_id 区分
		field.Enum("format").
			Values("nginx", "keepalived", "stream", "other").
			Default("nginx"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional(),
	}
}

func (ConfigFile) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("current_revision", ConfigRevision.Type).Unique(),
	}
}
