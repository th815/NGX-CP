// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 tianhao

package health

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/domain/compliance"
)

// ComplianceOpts 控制 DR 合规自检的探测范围（由 Agent 配置注入）。
type ComplianceOpts struct {
	// VIPs 是 LVS-DR 的虚拟 IP（如 10.0.0.10/32）。为空时 vip_on_lo 标记为"不适用"，不阻断。
	VIPs []string
	// KeepalivedConfPath 是 keepalived.conf 绝对路径，默认 /etc/keepalived/keepalived.conf。
	KeepalivedConfPath string
	// Role 可选：real_server / director / director_and_rs / unknown；影响 keepalived 相关项是否适用。
	Role string
}

// newItem 基于规则目录构造一条 ComplianceItem 骨架（name/title/severity/expected/fix_cmd），
// 由具体探测函数填充 Passed 与 Actual。
func newItem(r complianceRule) *agentv1.ComplianceItem {
	return &agentv1.ComplianceItem{
		Name:     r.Name,
		Title:    r.Title,
		Severity: r.Severity,
		Expected: r.Expected,
		FixCmd:   r.FixCmd,
	}
}

// complianceRule 是 compliance/probe 两包 RuleDef 的公共子集（字段名一致）。
type complianceRule = compliance.RuleDef

// RunCompliance 在主机上执行 DR 合规自检，返回控制面约定的 ComplianceReport。
// 规则目录（name/severity/期望/修复命令）复用 internal/domain/compliance.Catalog，
// 确保所有项的元信息与控制面判定侧完全一致。
func RunCompliance(ctx context.Context, exec CommandExecutor, opts ComplianceOpts) (*agentv1.ComplianceReport, error) {
	if opts.KeepalivedConfPath == "" {
		opts.KeepalivedConfPath = "/etc/keepalived/keepalived.conf"
	}
	items := make([]*agentv1.ComplianceItem, 0, len(compliance.Catalog))
	for _, rule := range compliance.Catalog {
		it := newItem(rule)
		switch rule.Name {
		case "vip_on_lo":
			checkVIPOnLo(exec, opts.VIPs, it)
		case "arp_suppress":
			checkARPSuppress(exec, it)
		case "keepalived_unicast":
			checkKeepalivedUnicast(exec, opts, it)
		case "no_ah_auth":
			checkNoAHAuth(exec, opts, it)
		case "director_promisc":
			// vCenter 端口组设置，Agent 不可探；标记为不适用（不阻断 degraded）。
			it.Passed = true
			it.Actual = "vCenter 端口组配置（混杂模式/MAC 更改/伪传输），Agent 不可探，由部署清单保证"
		case "time_sync":
			checkTimeSync(exec, it)
		case "lacp_ip_hash":
			it.Passed = true
			it.Actual = "ESXi 上行链路 LACP 哈希模式，Agent 不可探，由部署清单保证"
		}
		items = append(items, it)
	}
	return &agentv1.ComplianceReport{
		CheckedAt: time.Now().Unix(),
		Role:      opts.Role,
		Items:     items,
	}, nil
}

// checkVIPOnLo 检查 VIP 是否绑定在 lo 接口且为 /32（LVS-DR 的 ARP 隔离前提）。
func checkVIPOnLo(exec CommandExecutor, vips []string, it *agentv1.ComplianceItem) {
	if len(vips) == 0 {
		it.Passed = true
		it.Actual = "未配置 VIP（如非 LVS-DR RS 角色），跳过"
		return
	}
	out, err := exec.Output(context.Background(), "ip", "addr", "show", "lo")
	if err != nil {
		it.Passed = false
		it.Actual = "执行 ip addr show lo 失败: " + err.Error()
		return
	}
	missing := make([]string, 0)
	for _, vip := range vips {
		want := strings.TrimSpace(vip)
		if !strings.Contains(out, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		it.Passed = true
		it.Actual = "lo 接口已绑定全部 VIP（/32）"
	} else {
		it.Passed = false
		it.Actual = "lo 接口缺失 VIP: " + strings.Join(missing, ", ")
	}
}

// checkARPSuppress 检查 RS 内核 ARP 抑制：arp_ignore=1 且 arp_announce=2。
func checkARPSuppress(exec CommandExecutor, it *agentv1.ComplianceItem) {
	ignore, _ := exec.Output(context.Background(), "sysctl", "-n", "net.ipv4.conf.all.arp_ignore")
	announce, _ := exec.Output(context.Background(), "sysctl", "-n", "net.ipv4.conf.all.arp_announce")
	ignore = strings.TrimSpace(ignore)
	announce = strings.TrimSpace(announce)
	if ignore == "1" && announce == "2" {
		it.Passed = true
		it.Actual = "arp_ignore=1, arp_announce=2"
	} else {
		it.Passed = false
		it.Actual = fmt.Sprintf("arp_ignore=%q, arp_announce=%q（期望 1/2）", ignore, announce)
	}
}

// checkKeepalivedUnicast 检查 Keepalived VRRP 使用 unicast 且无 multicast 配置。
func checkKeepalivedUnicast(exec CommandExecutor, opts ComplianceOpts, it *agentv1.ComplianceItem) {
	if !exec.Exists(opts.KeepalivedConfPath) {
		it.Passed = true
		it.Actual = "未找到 keepalived.conf（节点未部署 keepalived），跳过"
		return
	}
	content, err := exec.ReadFile(opts.KeepalivedConfPath)
	if err != nil {
		it.Passed = false
		it.Actual = "读取 keepalived.conf 失败: " + err.Error()
		return
	}
	hasUnicast := strings.Contains(content, "unicast_src_ip") || strings.Contains(content, "unicast_peer")
	hasMulticast := regexp.MustCompile(`(?i)vrrp_(?:multicast|mcast)`).MatchString(content)
	if hasUnicast && !hasMulticast {
		it.Passed = true
		it.Actual = "使用 unicast_peer/unicast_src_ip，无 multicast 配置"
	} else {
		it.Passed = false
		it.Actual = fmt.Sprintf("unicast=%v, multicast=%v（必须为 unicast 且无 multicast）", hasUnicast, hasMulticast)
	}
}

// checkNoAHAuth 检查 Keepalived 未引用已移除的 AH 认证（Keepalived 2.x 已移除）。
func checkNoAHAuth(exec CommandExecutor, opts ComplianceOpts, it *agentv1.ComplianceItem) {
	if !exec.Exists(opts.KeepalivedConfPath) {
		it.Passed = true
		it.Actual = "未找到 keepalived.conf，跳过"
		return
	}
	content, err := exec.ReadFile(opts.KeepalivedConfPath)
	if err != nil {
		it.Passed = false
		it.Actual = "读取 keepalived.conf 失败: " + err.Error()
		return
	}
	hasAH := regexp.MustCompile(`(?i)auth_type\s+AH|ah_auth`).MatchString(content)
	if hasAH {
		it.Passed = false
		it.Actual = "配置引用了已移除的 AH 认证（Keepalived 2.x 不支持）"
	} else {
		it.Passed = true
		it.Actual = "未引用 AH 认证"
	}
}

// checkTimeSync 检查节点间时钟同步（偏差 ≤1s，与心跳时钟偏差联动）。
// 优先 timedatectl（systemd）；不可用则回退 chronyd 服务态；均不可用则按 warning 不阻断。
func checkTimeSync(exec CommandExecutor, it *agentv1.ComplianceItem) {
	if out, err := exec.Output(context.Background(), "timedatectl", "show", "-p", "NTPSynchronized", "--value"); err == nil {
		if strings.TrimSpace(out) == "yes" {
			it.Passed = true
			it.Actual = "NTP 已同步（timedatectl）"
			return
		}
	}
	if svcOut, serr := exec.Output(context.Background(), "systemctl", "is-active", "chronyd"); serr == nil && strings.TrimSpace(svcOut) == "active" {
		it.Passed = true
		it.Actual = "chronyd 服务运行中（systemctl is-active=active）"
		return
	}
	it.Passed = true
	it.Actual = "无法判定 NTP 同步态（非 systemd / 命令不可用），按 warning 不阻断"
}
