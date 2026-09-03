package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// ConfigSnapshot 是某次发布前在节点上抓取的配置快照（落地到本地目录 tar.gz）。
// 用于灰度回滚：回滚 = 恢复快照 + reload + 恢复权重。
type ConfigSnapshot struct {
	ent.Schema
}

func (ConfigSnapshot) Fields() []ent.Field {
	return []ent.Field{
		field.String("path"),  // 被快照的配置在节点上的路径，如 /etc/nginx/nginx.conf
		field.Enum("kind").
			Values("nginx", "keepalived", "stream", "other").
			Default("nginx"),
		field.Int("size").Default(0),
		field.String("checksum"), // SHA256
		field.String("stored_path"), // 本地快照文件绝对路径 /var/lib/ngxcp/snapshots/<node>/<ts>.tar.gz
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (ConfigSnapshot) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("node", Node.Type).
			Ref("snapshots").
			Unique(),
	}
}
