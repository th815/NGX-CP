// Package compliance 实现控制面侧的 DR 合规自检目录与判定。
//
// 实际探测由 Agent 在主机上执行（ip / arp / sysctl / keepalived.conf / 时钟偏差等），
// 控制面只持有「规则目录（单一事实来源）+ 聚合判定」，并据此驱动节点状态机：
//   online --(关键项不通过)--> degraded（见 docs/tasks/M1-skeleton.md T015 状态机）。
//
// 规则来自项目决策（DR 合规 6 项硬约束 + Keepalived/虚拟化前置陷阱），是 LVS-DR 发布的前置闸门。
package compliance

import agentv1 "github.com/th/ngxcp/gen/agent/v1"

// 严重级别。关键（critical）项不通过即判定整体不合规 → 节点降级为 degraded。
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

// 规则分类（便于前端分组展示与按类聚合）。
const (
	CatDR           = "dr"            // LVS-DR 网络层硬约束
	CatKeepalived   = "keepalived"    // Keepalived / VRRP 配置
	CatVirtualization = "virtualization" // 虚拟化端口组前置（vSphere 万兆 LACP）
	CatTimeSync     = "time_sync"     // 节点间时钟同步
)

// RuleDef 是一条 DR 合规规则的定义（控制面侧目录，不随节点变化）。
type RuleDef struct {
	Name     string
	Title    string
	Severity string // SeverityCritical / SeverityWarning
	Category string
	Expected string // 期望状态（人类可读）
	FixCmd   string // 修复命令建议（下发到 Agent 侧或运维参考）
}

// Catalog 是 DR 合规自检的完整规则目录（单一事实来源）。
//
// 覆盖：LVS-DR 网络层（VIP on lo /32、RS ARP 抑制）、Keepalived（unicast 替代组播、
// 不引用已移除的 AH 认证）、虚拟化前置（Director 端口组三项、LACP 基于 IP 哈希）、
// 时钟同步（偏差 ≤1s，与心跳时钟偏差联动）。
var Catalog = []RuleDef{
	{
		Name:     "vip_on_lo",
		Title:    "VIP 绑定在 lo 接口且掩码 /32",
		Severity: SeverityCritical,
		Category: CatDR,
		Expected: "ip addr show lo 含 <VIP>/32",
		FixCmd:   "ip addr add <VIP>/32 dev lo",
	},
	{
		Name:     "arp_suppress",
		Title:    "RS 内核 arp_ignore/arp_announce 抑制 ARP",
		Severity: SeverityCritical,
		Category: CatDR,
		Expected: "net.ipv4.conf.all.arp_ignore=1 且 arp_announce=2",
		FixCmd:   "sysctl -w net.ipv4.conf.all.arp_ignore=1 net.ipv4.conf.all.arp_announce=2",
	},
	{
		Name:     "keepalived_unicast",
		Title:    "Keepalived VRRP 使用 unicast（云禁组播）",
		Severity: SeverityCritical,
		Category: CatKeepalived,
		Expected: "keepalived.conf 含 unicast_src_ip/unicast_peer，无 multicast 配置",
		FixCmd:   "改用 unicast_peer 配置 VRRP 实例",
	},
	{
		Name:     "no_ah_auth",
		Title:    "未引用已移除的 AH 认证（Keepalived 2.x）",
		Severity: SeverityCritical,
		Category: CatKeepalived,
		Expected: "配置中无 auth_type AH / ah_auth 段落",
		FixCmd:   "删除 AH 认证配置，改用其他鉴权或不依赖 VRRP 认证",
	},
	{
		Name:     "director_promisc",
		Title:    "Director 端口组开启 混杂模式+MAC地址更改+伪传输",
		Severity: SeverityCritical,
		Category: CatVirtualization,
		Expected: "vSwitch 端口组三项均启用（否则 ipvsadm 计数正常但 RS 收不到包）",
		FixCmd:   "在 vCenter 对应端口组启用 promiscuous / mac_changes / forged_transmits",
	},
	{
		Name:     "time_sync",
		Title:    "节点间时钟同步（偏差 ≤1s）",
		Severity: SeverityWarning,
		Category: CatTimeSync,
		Expected: "两两偏差 ≤1s（chrony 已启用，禁用 VMware Tools 时间同步）",
		FixCmd:   "启用 chrony 并关闭 VMware Tools 时间同步",
	},
	{
		Name:     "lacp_ip_hash",
		Title:    "LACP 哈希基于 IP（用满万兆）",
		Severity: SeverityWarning,
		Category: CatVirtualization,
		Expected: "ESXi 上行链路 LACP 负载模式=源+目的 IP",
		FixCmd:   "修改 LACP 哈希算法为基于 IP",
	},
}

// CatalogByName 便于按规则名快速索引。
func CatalogByName() map[string]RuleDef {
	m := make(map[string]RuleDef, len(Catalog))
	for _, r := range Catalog {
		m[r.Name] = r
	}
	return m
}

// FindItem 在 Agent 上报的报告里按 name 找对应项（找不到返回 nil）。
func FindItem(report *agentv1.ComplianceReport, name string) *agentv1.ComplianceItem {
	if report == nil {
		return nil
	}
	for _, it := range report.GetItems() {
		if it.GetName() == name {
			return it
		}
	}
	return nil
}
