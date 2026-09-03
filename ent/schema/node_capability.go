package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// NodeCapability 是纳管节点时执行 `nginx -V` 解析出的能力基线画像，
// 用于配置校验时的"能力边界"检查与双机编译一致性 diff。
type NodeCapability struct {
	ent.Schema
}

func (NodeCapability) Fields() []ent.Field {
	return []ent.Field{
		// ---- 主机身份（T018 补齐：节点详情页「系统信息」Tab 的数据源）----
		field.String("hostname").Optional(),
		field.String("os").Optional(),           // "rocky 9.4"
		field.String("kernel").Optional(),
		field.Bool("has_keepalived").Default(false), // Director 角色的判定依据之一
		field.Bool("has_ipvsadm").Default(false),

		// ---- nginx 能力基线 ----
		field.String("version").Optional(), // 1.30.0（非 nginx 节点为空）
		field.String("prefix").Optional(),  // /etc/nginx
		field.String("conf_path").Optional(),
		field.String("sbin_path").Optional(),
		// 编译模块清单（http_ssl/http_v2/stream/...），JSON 数组存储
		field.JSON("modules", []string{}).Optional(),
		field.Text("raw_args").Optional(),      // 完整 configure 参数
		field.String("config_hash").Optional(), // nginx -T 全量配置的哈希（T017）
		field.String("checksum").Optional(),    // 整份画像的哈希，便于快速比对双机一致性
		// 主机运行底座画像（OS/内核/SELinux/ulimit/磁盘等）的 JSON 序列化（T018），
		// 单字段持有避免对 SystemInfo 的每个子项都建列；取不到时为空串（未采集）。
		field.String("system_info").Optional(),
		field.Time("captured_at").Optional(),   // 采集时间
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (NodeCapability) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("node", Node.Type).
			Ref("capabilities").
			Unique(),
	}
}
