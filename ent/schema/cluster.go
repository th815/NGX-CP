package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// Cluster 是一组节点的逻辑分组（例如 prod-web）。
// 被 Node 的 cluster 边引用，是 Node 反向边的另一端。
type Cluster struct {
	ent.Schema
}

func (Cluster) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").Unique(),
		field.String("description").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional(),
	}
}

func (Cluster) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("nodes", Node.Type),
	}
}
