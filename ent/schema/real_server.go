package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/edge"
)

// RealServer 对应 LVS DR 架构下一个真实服务器（RS）条目：
// ipvsadm -a -t <VIP>:<port> -r <RS_IP>:<port> -g -w <weight>
// 一个 Node 可对应多条 RS 记录（不同 VIP/端口）。
type RealServer struct {
	ent.Schema
}

func (RealServer) Fields() []ent.Field {
	return []ent.Field{
		field.String("vip"),  // 虚拟服务地址，如 10.0.0.10
		field.Int("vport"),   // 虚拟服务端口
		field.String("rip"),  // 真实服务器 IP（通常即 Node 地址）
		field.Int("rport"),   // 真实服务器端口（DR 模式需与 vport 一致）
		field.Int("weight").Default(1),
		field.Bool("enabled").Default(true),
		field.Enum("protocol").
			Values("tcp", "udp").
			Default("tcp"),
		field.Enum("state").
			Values("active", "draining", "down").
			Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional(),
	}
}

func (RealServer) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("node", Node.Type).
			Ref("real_servers").
			Unique(),
	}
}
