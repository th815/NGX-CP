package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// VirtualService 对应一个 LVS 虚拟服务（VIP:port），挂在某个 Director 下。
// DR 模式不支持端口映射，故 vport 必须等于后端 RS 的 rport。
type VirtualService struct {
	ent.Schema
}

func (VirtualService) Fields() []ent.Field {
	return []ent.Field{
		field.String("vip").Comment("虚拟服务地址，如 192.168.5.5"),
		field.Int("port").Comment("虚拟服务端口（DR 模式下必须等于 RS 端口）"),
		field.Enum("protocol").
			Values("tcp", "udp").
			Default("tcp"),
		field.Enum("scheduler").
			Values("rr", "wrr", "lc", "wlc").
			Default("wrr").
			Comment("调度算法：rr 轮询 / wrr 加权轮询 / lc 最少连接 / wlc 加权最少连接"),
		field.Bool("enabled").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (VirtualService) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("director", Director.Type).
			Ref("virtual_services").
			Unique().
			Required(),
	}
}
