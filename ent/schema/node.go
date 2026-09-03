package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// Node 表示一个被纳管的节点（Nginx 真实服务器 / LVS Director / 两者兼具）。
// 完整字段见 docs/tasks/M0-foundation.md T006 契约。
type Node struct {
	ent.Schema
}

func (Node) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").Unique(), // rs-nginx-01
		field.String("address"),       // 10.0.1.11
		field.Enum("role").
			Values("real_server", "director", "director_and_rs", "unknown"),
		field.Enum("status").
			Values("online", "offline", "degraded", "enrolling", "decommissioned"),
		field.Int("lvs_weight").Default(1),
		field.Bool("lvs_enabled").Default(true),
		field.Time("last_heartbeat_at").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional(), // 软删除
	}
}

func (Node) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("capabilities", NodeCapability.Type),
		edge.To("config_files", NodeConfigFile.Type),
		edge.To("log_targets", NodeLogTarget.Type),
		edge.To("snapshots", ConfigSnapshot.Type),
		edge.To("deploy_tasks", DeployTask.Type),
		edge.To("real_servers", RealServer.Type),
		edge.From("cluster", Cluster.Type).
			Ref("nodes").
			Unique(),
	}
}
