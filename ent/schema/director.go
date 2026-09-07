package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Director 对应一台 LVS Director（Keepalived 节点）。
// 双机仅 state / priority / unicast_src_ip 三项不同，其余必须完全一致。
type Director struct {
	ent.Schema
}

func (Director) Fields() []ent.Field {
	return []ent.Field{
		field.Enum("state").
			Values("MASTER", "BACKUP").
			Comment("VRRP 状态；双机一主一备"),
		field.Int("priority").Comment("VRRP 优先级；MASTER 高（如 150）、BACKUP 低（如 100）"),
		field.Int("virtual_router_id").Comment("virtual_router_id，同二层必须唯一（0-255）"),
		field.String("unicast_src_ip").Comment("VRRP unicast 源地址（云环境禁组播，必须 unicast）"),
		field.String("unicast_peer_ip").Comment("对端 Director 的 unicast 地址"),
		field.String("iface").Default("eth0").Comment("承载 VRRP 的物理网卡"),
		field.Enum("mode").
			Values("DR", "NAT", "TUN").
			Default("DR").
			Comment("LVS 转发模式；本项目为 DR"),
		field.String("vip").Comment("该 Director 持有的虚拟 IP（lo:0，/32）"),
		field.Bool("holding_vip").Default(false).Comment("实时：当前是否持有 VIP（主备切换判据，T056）"),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (Director) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("node", Node.Type).
			Ref("directors").
			Unique().
			Required(),
		edge.To("virtual_services", VirtualService.Type),
	}
}
