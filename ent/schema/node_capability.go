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
		field.String("version"),  // 1.30.0
		field.String("prefix"),   // /etc/nginx
		field.String("conf_path"), // /etc/nginx/nginx.conf
		field.String("sbin_path"),  // /usr/sbin/nginx
		// 编译模块清单（http_ssl/http_v2/stream/...），JSON 数组存储
		field.JSON("modules", []string{}),
		field.Text("raw_args"),     // 完整 configure 参数
		field.String("checksum"),   // 整份画像的哈希，便于快速比对双机一致性
		field.Time("captured_at"),  // 采集时间
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
