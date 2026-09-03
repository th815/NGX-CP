package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// DeployTask 是某个 ChangeOrder 在单个节点上的发布任务。
// 对应 Node 的 deploy_tasks 边、ChangeOrder 的 deploy_tasks 边。
type DeployTask struct {
	ent.Schema
}

func (DeployTask) Fields() []ent.Field {
	return []ent.Field{
		field.Enum("state").
			Values("pending", "running", "succeeded", "failed", "rolled_back", "skipped").
			Default("pending"),
		// 当前所处阶段：prepare → validate → canary → observe → rollback
		field.Enum("phase").
			Values("prepare", "validate", "canary", "observe", "rollback").
			Optional(),
		field.Int("attempts").Default(0),
		field.Text("error_detail").Optional(), // 失败时的命令输出/错误信息（含 nginx -t 输出）
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
