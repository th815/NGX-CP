package lvs

import (
	"context"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/director"
	"github.com/th/ngxcp/ent/realserver"
	"github.com/th/ngxcp/ent/virtualservice"
)

// Service 聚合 LVS 拓扑、渲染配置、门禁判定的控制面服务。
type Service struct {
	client *ent.Client
}

// New 构造 LVS 服务。
func New(client *ent.Client) *Service {
	return &Service{client: client}
}

// DirectorNode 是拓扑里一个 Director 的视图（融合节点主机信息）。
type DirectorNode struct {
	ID         int    `json:"id"`
	NodeID     int    `json:"node_id"`
	Address    string `json:"address"`
	Role       string `json:"role"`
	Status     string `json:"status"`
	State      string `json:"state"`       // MASTER / BACKUP
	HoldingVIP bool   `json:"holding_vip"` // 实时：当前是否持有 VIP（主备切换判据，T056 填充）
	Priority   int    `json:"priority"`
	VRID       int    `json:"virtual_router_id"`
}

// RealServerNode 是拓扑里一个 RS 的视图（融合节点主机信息）。
type RealServerNode struct {
	NodeID  int    `json:"node_id"`
	Address string `json:"address"`
	RIP     string `json:"rip"`
	RPort   int    `json:"rport"`
	Weight  int    `json:"weight"`
	Enabled bool   `json:"enabled"`
	State   string `json:"state"` // active / draining / down
	VSKey   string `json:"vs_key"`
}

// Topology 是 LVS 拓扑聚合结果（Client → VIP → Director×2 → RS×N）。
type Topology struct {
	VIP       string           `json:"vip"`
	Directors []DirectorNode   `json:"directors"`
	RS        []RealServerNode `json:"rs"`
}

// Topology 聚合模型 + 节点在线状态为前端拓扑图数据。
// 注意：本接口只读模型 + 节点状态；实时 VIP 持有（holding_vip）由 T056 经 Agent
// 合规上报填充，未填充时取 Director.state 推断（MASTER 即持有）。
func (s *Service) Topology(ctx context.Context) (*Topology, error) {
	directors, err := s.client.Director.Query().
		WithNode().
		Order(ent.Asc(director.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	topo := &Topology{}
	for _, d := range directors {
		dn := DirectorNode{
			ID:         d.ID,
			State:      string(d.State),
			HoldingVIP: d.HoldingVip,
			Priority:   d.Priority,
			VRID:       d.VirtualRouterID,
		}
		if d.Edges.Node != nil {
			dn.NodeID = d.Edges.Node.ID
			dn.Address = d.Edges.Node.Address
			dn.Role = string(d.Edges.Node.Role)
			dn.Status = string(d.Edges.Node.Status)
		}
		// 未上报 holding_vip 时按 state 推断：MASTER 视为持有。
		if !d.HoldingVip {
			dn.HoldingVIP = string(d.State) == "MASTER"
		}
		if topo.VIP == "" {
			topo.VIP = d.Vip
		}
		topo.Directors = append(topo.Directors, dn)
	}

	// RS 来自节点级 real_server（每 RS 带 vip/vport，关联到 VS）。
	rsList, err := s.client.RealServer.Query().
		WithNode().
		Order(ent.Asc(realserver.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rsList {
		rn := RealServerNode{
			RIP:     r.Rip,
			RPort:   r.Rport,
			Weight:  r.Weight,
			Enabled: r.Enabled,
			VSKey:   realserverVSKey(r.Vip, r.Vport),
		}
		if r.State != "" {
			rn.State = string(r.State)
		}
		if r.Edges.Node != nil {
			rn.NodeID = r.Edges.Node.ID
			rn.Address = r.Edges.Node.Address
		}
		topo.RS = append(topo.RS, rn)
	}

	return topo, nil
}

func realserverVSKey(vip string, vport int) string {
	return vip + ":" + itoa(vport)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// VirtualServices 返回全部虚拟服务（含所属 Director 信息），供 UI 表格展示。
func (s *Service) VirtualServices(ctx context.Context) ([]VirtualServiceView, error) {
	vsList, err := s.client.VirtualService.Query().
		WithDirector().
		Order(ent.Asc(virtualservice.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]VirtualServiceView, 0, len(vsList))
	for _, v := range vsList {
		vv := VirtualServiceView{
			ID:       v.ID,
			VIP:      v.Vip,
			Port:     v.Port,
			Protocol: string(v.Protocol),
			Scheduler: string(v.Scheduler),
			Enabled:  v.Enabled,
		}
		if v.Edges.Director != nil {
			vv.DirectorID = v.Edges.Director.ID
			vv.DirectorState = string(v.Edges.Director.State)
		}
		out = append(out, vv)
	}
	return out, nil
}

// VirtualServiceView 是虚拟服务的视图。
type VirtualServiceView struct {
	ID           int    `json:"id"`
	DirectorID   int    `json:"director_id"`
	DirectorState string `json:"director_state"`
	VIP          string `json:"vip"`
	Port         int    `json:"port"`
	Protocol     string `json:"protocol"`
	Scheduler    string `json:"scheduler"`
	Enabled      bool   `json:"enabled"`
}
