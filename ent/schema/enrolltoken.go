package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// EnrollToken 是「预建节点一次性接入令牌」，对应 T014 预建节点注册流程：
// 控制面先建节点，再为其签发 enroll_token，Agent 持令牌 + 本地 CSR 换取客户端证书。
//
// 与「节点绑定自注册令牌」JoinToken 的职责边界（两者并存、互不替代）：
//   - EnrollToken：一次性（used 标志）。签发即绑定节点，校验成功后即作废，不能再用于注册。
//     适用于「控制面预建节点 → 下发令牌给 Agent 首次注册」的场景。
//   - JoinToken：可复用（无 used 标志）。控制面不存明文，Agent 侧持久化（/etc/ngxcp/agent.conf），
//     凭同一令牌可反复重建证书（证书丢失场景），契合 web 一键自注册。
//
// 两者都持久化于服务端（入库），都支持主动吊销（revoked 即时失效），
// 解决早期 in-memory 方案「重启即丢、不可吊销」的隐患，满足生产持久化与可审计要求。
//
// 安全：库内只存 SHA-256 哈希（token_hash），原文仅在签发时返回一次。
type EnrollToken struct {
	ent.Schema
}

func (EnrollToken) Fields() []ent.Field {
	return []ent.Field{
		field.String("token_hash").
			Unique().
			Comment("SHA-256(原始令牌)，唯一；库内不存明文"),
		field.Time("expires_at").
			Comment("令牌过期时间；过期即拒绝"),
		field.Bool("used").
			Default(false).
			Comment("一次性使用标志；true 即失效（不可复用）"),
		field.Bool("revoked").
			Default(false).
			Comment("吊销标志；true 即失效（预留：后期授权/商业化可据节点数策略吊销）"),
		field.Time("used_at").
			Optional().
			Comment("使用时间，供审计"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (EnrollToken) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("node", Node.Type).
			Ref("enroll_tokens").
			Unique().
			Required().
			Comment("令牌绑定的节点（1 token = 1 node）"),
	}
}
