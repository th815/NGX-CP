// Package lvs 实现 LVS / Keepalived 的控制面逻辑：配置渲染、拓扑聚合、合规门禁。
// 设计原则：渲染器是纯函数（无 IO），便于单测对照 testdata；双机仅 3 项差异。
package lvs

import (
	"fmt"
	"sort"
	"strings"
)

// RenderDirector 是渲染 Keepalived 所需的最小 Director 视图（来自数据模型 + 节点地址）。
type RenderDirector struct {
	State           string // "MASTER" | "BACKUP"
	Priority        int    // VRRP 优先级
	VirtualRouterID int    // 同二层必须唯一
	UnicastSrcIP    string // 本机 unicast 源
	UnicastPeerIP   string // 对端 Director unicast 地址
	Interface       string // 承载 VRRP 的物理网卡
	Mode            string // "DR" | "NAT" | "TUN"
	VIP             string // 该 Director 持有的虚拟 IP
	VRRPInstance    string // vrrp 实例名，通常 "VI_1"
}

// RenderVS 是虚拟服务的最小视图。
type RenderVS struct {
	VIP      string
	Port     int
	Protocol string // tcp | udp
	Scheduler string // rr | wrr | lc | wlc
}

// RenderRS 是真实服务器的最小视图。
type RenderRS struct {
	RIP    string
	RPort  int
	Weight int
	Enabled bool
}

// RenderKeepalived 从 Director + VS 列表 + 按 VIP 归组的 RS，渲染标准 keepalived.conf。
// 关键约束（见 docs/DECISIONS.md / ARCHITECTURE §7）：
//   - MASTER/BACKUP 仅 state / priority / unicast_src_ip（及 unicast_peer 互换）三处不同；
//   - DR 模式不支持端口映射，VS 端口必须等于 RS 端口（调用方负责保证）；
//   - virtual_router_id 同二层必须唯一（见 CheckVirtualRouterIDConflicts）。
func RenderKeepalived(d RenderDirector, vs []RenderVS, rsByVS map[string][]RenderRS) string {
	var b strings.Builder

	// global_defs
	b.WriteString("global_defs {\n")
	b.WriteString(fmt.Sprintf("    router_id ngxcp-%s\n", d.VRRPInstance))
	b.WriteString("    enable_script_security\n")
	b.WriteString("}\n\n")

	// vrrp_instance
	b.WriteString(fmt.Sprintf("vrrp_instance %s {\n", d.VRRPInstance))
	b.WriteString(fmt.Sprintf("    state %s\n", d.State))
	b.WriteString(fmt.Sprintf("    interface %s\n", d.Interface))
	b.WriteString(fmt.Sprintf("    virtual_router_id %d\n", d.VirtualRouterID))
	b.WriteString(fmt.Sprintf("    priority %d\n", d.Priority))
	b.WriteString("    advert_int 1\n")
	b.WriteString(fmt.Sprintf("    unicast_src_ip %s\n", d.UnicastSrcIP))
	b.WriteString("    unicast_peer {\n")
	b.WriteString(fmt.Sprintf("        %s\n", d.UnicastPeerIP))
	b.WriteString("    }\n")
	b.WriteString("    virtual_ipaddress {\n")
	b.WriteString(fmt.Sprintf("        %s/32 dev lo\n", d.VIP))
	b.WriteString("    }\n")
	b.WriteString("}\n\n")

	// virtual_server 块，按 "VIP:Port" 稳定排序（同 VIP 多端口不能互相覆盖）。
	keys := make([]string, 0, len(vs))
	vsByKey := make(map[string]RenderVS, len(vs))
	for _, v := range vs {
		k := fmt.Sprintf("%s:%d", v.VIP, v.Port)
		keys = append(keys, k)
		vsByKey[k] = v
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := vsByKey[k]
		proto := strings.ToUpper(v.Protocol)
		if proto == "" {
			proto = "TCP"
		}
		kind := strings.ToUpper(d.Mode)
		if kind == "" {
			kind = "DR"
		}
		b.WriteString(fmt.Sprintf("virtual_server %s %d {\n", v.VIP, v.Port))
		b.WriteString("    delay_loop 5\n")
		b.WriteString(fmt.Sprintf("    lb_algo %s\n", v.Scheduler))
		b.WriteString(fmt.Sprintf("    lb_kind %s\n", kind))
		b.WriteString(fmt.Sprintf("    protocol %s\n", proto))
		// RS 按 "RIP:RPort" 稳定排序
		rsKey := fmt.Sprintf("%s:%d", v.VIP, v.Port)
		rsList := rsByVS[rsKey]
		sort.Slice(rsList, func(i, j int) bool { return rsList[i].RIP < rsList[j].RIP })
		for _, rs := range rsList {
			if !rs.Enabled {
				continue
			}
			b.WriteString(fmt.Sprintf("    real_server %s %d {\n", rs.RIP, rs.RPort))
			b.WriteString(fmt.Sprintf("        weight %d\n", rs.Weight))
			b.WriteString("        TCP_CHECK {\n")
			b.WriteString("            connect_timeout 3\n")
			b.WriteString("        }\n")
			b.WriteString("    }\n")
		}
		b.WriteString("}\n\n")
	}
	return b.String()
}

// CheckVirtualRouterIDConflicts 检测同二层 virtual_router_id 冲突（返回冲突的 VRID 列表）。
// 约束见 ARCHITECTURE §7：virtual_router_id 同二层必须唯一，否则双主脑裂。
func CheckVirtualRouterIDConflicts(directors []RenderDirector) []int {
	seen := make(map[int]int) // vrid -> 出现次数
	for _, d := range directors {
		seen[d.VirtualRouterID]++
	}
	conflicts := make([]int, 0)
	for vrid, n := range seen {
		if n > 1 {
			conflicts = append(conflicts, vrid)
		}
	}
	sort.Ints(conflicts)
	return conflicts
}
