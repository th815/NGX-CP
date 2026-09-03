package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// ConfigRevision 是某节点某路径的一个配置版本。
// 通过 parent 自边串成版本链；通过 blob 边引用实际内容（去重存储）。
// node_id 以字段形式保存（不占用 Node 的边，保持 Node 边集合与 T006 契约一致）。
type ConfigRevision struct {
	ent.Schema
}

func (ConfigRevision) Fields() []ent.Field {
	return []ent.Field{
		field.Int("node_id"),   // 关联节点
		field.String("path"),   // 配置在节点上的路径，如 /etc/nginx/conf.d/upstream.conf
		field.Enum("source").
			Values("sync", "manual_edit", "cert_renew", "security_block", "rollback").
			Default("sync"), // 版本来源：Agent 同步 / 手动编辑 / 证书续期 / 安全封禁 / 回滚
		field.Int("change_order_id").Optional(), // 关联变更单（发布产生的版本填写）
		field.String("message").Optional(),      // 变更说明
		field.String("author").Optional(),       // 操作人/来源标识（如 "agent" / 用户名）
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (ConfigRevision) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("blob", ConfigBlob.Type).
			Ref("revisions").
			Unique(),
		// 版本链：每个版本指向一个父版本（O2M —— 一个父版本可有多个子版本）
		edge.From("parent", ConfigRevision.Type).
			Ref("children").
			Unique(),
		edge.To("children", ConfigRevision.Type),
	}
}
