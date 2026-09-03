package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// NodeLogTarget 是从 nginx 配置提取出的日志采集目标（T018 产出）。
//
// 回答「Agent 该 tail 哪些文件」：
//   - is_off / is_syslog 的目标跳过采集（skip_reason 说明原因，UI 直接展示）；
//   - has_variable 的路径不做通配展开，只告警交人工处理；
//   - inode 用于检测 logrotate 轮转——文件被重命名重建后必须重新打开，
//     否则 Agent 会一直 tail 已轮转的旧文件。
//
// 与 NodeConfigFile 同理，每次采集整体替换（快照语义）。
type NodeLogTarget struct {
	ent.Schema
}

func (NodeLogTarget) Fields() []ent.Field {
	return []ent.Field{
		field.String("path"),         // /var/log/nginx/access.log（off/syslog 时为空）
		field.Enum("type").Values("access", "error"),
		field.String("format").Optional(),      // main | json（仅 access_log）
		field.String("level").Optional(),       // warn | error（仅 error_log）
		field.Bool("is_syslog").Default(false),
		field.Bool("is_off").Default(false),
		field.Bool("has_variable").Default(false),
		field.String("skip_reason").Optional(), // off | syslog | stderr | memory
		field.Int64("size").Default(-1),        // 字节；stat 失败为 -1
		field.Uint64("inode").Default(0),
		field.String("stat_err").Optional(),
		field.Time("captured_at"),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (NodeLogTarget) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("node", Node.Type).
			Ref("log_targets").
			Unique().
			Required(),
	}
}

func (NodeLogTarget) Indexes() []ent.Index {
	return []ent.Index{
		// 同节点的同一路径+类型+跳过原因只保留一条（off/syslog 条目路径皆空，靠 skip_reason 区分）。
		index.Fields("path", "skip_reason").
			Edges("node"),
	}
}
