package lvs

import (
	"context"
	"strings"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/director"
	"github.com/th/ngxcp/ent/realserver"
	"github.com/th/ngxcp/ent/virtualservice"
	domainlvs "github.com/th/ngxcp/internal/domain/lvs"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// Service 聚合 LVS 拓扑、渲染配置、权重编排、门禁判定的控制面服务。
type Service struct {
	client *ent.Client
	setter domainlvs.WeightSetter // 远程权重执行器（Agent 通道），nil 时仅查询可用
	gate   *Gate                   // 发布前门禁（T055），nil 时放行
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
	ID      int    `json:"id"`
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
			ID:      r.ID,
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

// ===== T054：控制面权重编排（经 Agent 通道驱动 Director 节点上的 ipvsadm） =====

// SetWeightSetter 注入远程权重执行器（基于 transport.Server 的 Agent 命令通道）。
func (s *Service) SetWeightSetter(w domainlvs.WeightSetter) { s.setter = w }

// SetGate 注入发布前门禁（T055）；nil 时放行。
func (s *Service) SetGate(g *Gate) { s.gate = g }

// RealServerView 是单条 RS 记录的操作结果视图。
type RealServerView struct {
	ID      int    `json:"id"`
	RIP     string `json:"rip"`
	RPort   int    `json:"rport"`
	VIP     string `json:"vip"`
	VPort   int    `json:"vport"`
	Weight  int    `json:"weight"`
	Enabled bool   `json:"enabled"`
	NodeID  int    `json:"node_id"`
}

// realServersForRIP 按 rsID 找到其 rip，并返回该 rip 关联的全部 RS 记录（含 Node 边）。
// 生产一个 RS 常同时挂在 :80 / :443(tcp) / :443(udp)，权重操作需对其全部条目统一进行。
func (s *Service) realServersForRIP(ctx context.Context, rsID int) ([]*ent.RealServer, error) {
	first, err := s.client.RealServer.Get(ctx, rsID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "RealServer 不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "查询 RealServer 失败", err)
	}
	recs, err := s.client.RealServer.Query().
		Where(realserver.Rip(first.Rip)).
		WithNode().
		All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询 RS 记录失败", err)
	}
	return recs, nil
}

// Drain 摘除：把 rip 关联的全部 RS 记录在其所属 VS 上权重置 0，模型标记 Enabled=false。
// 保留 Weight 基线（不覆盖），以便 Restore 精确加回。
func (s *Service) Drain(ctx context.Context, rsID int) error {
	recs, err := s.realServersForRIP(ctx, rsID)
	if err != nil {
		return err
	}
	return s.dispatch(ctx, recs, func(r *ent.RealServer) (target int, enabled, updateBaseline bool) {
		return 0, false, false
	})
}

// Restore 恢复：把 rip 关联的全部 RS 记录下发其基线 Weight，模型标记 Enabled=true。
func (s *Service) Restore(ctx context.Context, rsID int) error {
	recs, err := s.realServersForRIP(ctx, rsID)
	if err != nil {
		return err
	}
	return s.dispatch(ctx, recs, func(r *ent.RealServer) (target int, enabled, updateBaseline bool) {
		return r.Weight, true, false
	})
}

// SetBaselineWeight 设置基线权重并下发：更新模型 Weight=w、Enabled=w>0，并经 Agent 下发 w。
func (s *Service) SetBaselineWeight(ctx context.Context, rsID, w int) error {
	if w < 0 {
		return apperr.New(apperr.CodeInvalid, "权重不可为负")
	}
	recs, err := s.realServersForRIP(ctx, rsID)
	if err != nil {
		return err
	}
	return s.dispatch(ctx, recs, func(r *ent.RealServer) (target int, enabled, updateBaseline bool) {
		return w, w > 0, true
	})
}

// dispatch 把一组 RS 记录按 calc 给出的目标权重经 Agent 下发，并写回模型。
// calc 返回：(target 下发权重, enabled 模型启用态, updateBaseline 是否回写基线 Weight 字段)。
// 门禁（T055）：任一 RS 节点不通过则整体阻断，绝不部分下发。
func (s *Service) dispatch(ctx context.Context, recs []*ent.RealServer, calc func(r *ent.RealServer) (int, bool, bool)) error {
	if s.setter == nil {
		return apperr.New(apperr.CodeUnavailable, "控制面未接入 Agent 下发通道（权重执行器缺失）")
	}
	for _, r := range recs {
		if s.gate != nil && r.Edges.Node != nil {
			if err := s.gate.Check(ctx, r.Edges.Node.ID); err != nil {
				return err
			}
		}
		vs := domainlvs.VirtualServerRef{Proto: strings.ToUpper(string(r.Protocol)), Address: r.Vip, Port: r.Vport}
		rs := domainlvs.RealServerRef{Address: r.Rip, Port: r.Rport}
		target, enabled, updateBaseline := calc(r)
		if err := s.setter.SetWeight(ctx, vs, rs, target); err != nil {
			return err
		}
		upd := r.Update().SetEnabled(enabled)
		if updateBaseline {
			upd = upd.SetWeight(target)
		}
		if err := upd.Exec(ctx); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "更新 RS 记录失败", err)
		}
	}
	return nil
}
