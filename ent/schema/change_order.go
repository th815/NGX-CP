package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// ChangeOrder 是一次配置变更的"工单"，聚合多个节点的 DeployTask。
// 状态机：draft → pending → approved → applying → done / failed / rolled_back。
type ChangeOrder struct {
	ent.Schema
}

func (ChangeOrder) Fields() []ent.Field {
	return []ent.Field{
		field.String("title"),
		field.Text("description").Optional(),
		field.Enum("status").
			Values("draft", "pending", "approved", "rejected", "applying", "done", "failed", "rolled_back").
			Default("draft"),
		field.Enum("priority").
			Values("low", "normal", "high", "urgent").
			Default("normal"),
		field.String("created_by").Optional(),
		field.String("approved_by").Optional(),
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
