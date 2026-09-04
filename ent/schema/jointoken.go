package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// JoinToken 是「节点绑定自注册令牌」（仿妙妙屋X 的 token→节点 服务端映射）。
//
// 设计对齐 mmw：令牌即节点身份，签发时登记到库（token→node 映射），服务端按令牌
// 哈希反查节点；支持单独吊销（revoked 标志），吊销即时生效、无需等过期。
//
// 安全：库内只存 SHA-256 哈希（token_hash），原文仅在签发时返回一次，
// 持久化于 Agent 侧（/etc/ngxcp-agent.env，systemd EnvironmentFile）。
// 一个节点可有多条令牌（轮换时旧令牌置 revoked），但每条令牌仅绑定一个节点。
type JoinToken struct {
	ent.Schema
}

func (JoinToken) Fields() []ent.Field {
	return []ent.Field{
		field.String("token_hash").
			Unique().
			Comment("SHA-256(原始令牌)，唯一；库内不存明文"),
		field.Enum("role").
			Values("real_server", "director", "director_and_rs", "unknown").
			Comment("签发时锁定的节点角色"),
		field.Time("expires_at").
			Comment("令牌过期时间；enrolling 阶段过期即拒绝，已纳管节点不受影响"),
		field.Bool("revoked").
			Default(false).
			Comment("吊销标志；true 即失效（旋转/泄漏时立即吊销）"),
		field.Time("last_used_at").
			Optional().
			Comment("最近一次成功用于注册的时间，供审计/异常检测"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (JoinToken) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("node", Node.Type).
			Ref("join_tokens").
			Unique().
			Required().
			Comment("令牌绑定的节点（1 token = 1 node）"),
	}
}
