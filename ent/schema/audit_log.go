package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// AuditLog 是不可变审计日志，记录关键操作（纳管、发布、回滚、证书、登录等）。
// 安全审计依据，写入后不应修改。
type AuditLog struct {
	ent.Schema
}

func (AuditLog) Fields() []ent.Field {
	return []ent.Field{
		field.String("actor"), // 操作人 / 系统 / Agent
		field.Enum("action").
			Values(
				"node_create", "node_update", "node_delete", "node_enroll",
				"config_push", "config_deploy", "config_rollback",
				"cert_issue", "cert_renew", "lvs_change", "blocklist_apply",
				"login", "logout", "key_rotate",
			),
		field.String("target_type").Optional(), // node / change_order / cert / ...
		field.Int("target_id").Optional(),
		field.Text("detail").Optional(),
		field.String("ip").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}
