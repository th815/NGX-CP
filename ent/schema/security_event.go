package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"time"
)

// SecurityEvent 是安全检测命中的事件流（M6 T067）。
// 与 ClickHouse 的 security_alerts（T066 落库表）分层：CH 管命中原始时序，
// 本表（PG ent）管处置状态机（pending → blocked/ignored）。证据 Sample 存
// 原始日志片段，便于事后复盘（AI 陷阱：证据样本要存原始日志片段）。
// 事件一旦处置（handled=true）即状态锁，禁止重复动作（避免重复封禁/误伤）。
//
// 字段偏离 T067 草案说明（文档契约写 RuleID/NodeID 为 int，此处用 string）：
//   - rule_id 用字符串，因为 T066 规则 ID 是平台内置字符串（如 r-sql-injection），
//     int 会丢失映射且需额外查表；存字符串直接可读。
//   - node 用字符串，因为命中最相关日志来自日志的 node 字段（如 "n1"/主机名），
//     不是 ent 节点的自增 ID，避免字符串↔int 映射丢失。
type SecurityEvent struct {
	ent.Schema
}

func (SecurityEvent) Fields() []ent.Field {
	return []ent.Field{
		field.String("rule_id"), // 对应 security.Rule.ID（字符串）
		field.String("rule_name"),
		field.Enum("level").Values("INFO", "WARN", "CRITICAL"),
		field.String("node").Optional(), // 命中最相关日志的节点标识（取自日志 node 字段）
		field.Text("sample").Optional(), // 证据：原始日志片段（Entry.Raw 或整条序列化）
		field.Bool("handled").Default(false),
		field.Enum("action").Values("pending", "blocked", "ignored").Default("pending"),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (SecurityEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("handled"),
		index.Fields("level"),
		index.Fields("rule_id"),
	}
}
