package lvs

import (
	"strings"
	"testing"
)

func sampleVS() []RenderVS {
	return []RenderVS{
		{VIP: "192.168.5.5", Port: 80, Protocol: "tcp", Scheduler: "wrr"},
		{VIP: "192.168.5.5", Port: 443, Protocol: "tcp", Scheduler: "wrr"},
	}
}

func sampleRS() map[string][]RenderRS {
	return map[string][]RenderRS{
		"192.168.5.5:80": {
			{RIP: "192.168.5.8", RPort: 80, Weight: 1, Enabled: true},
			{RIP: "192.168.5.9", RPort: 80, Weight: 1, Enabled: true},
		},
		"192.168.5.5:443": {
			{RIP: "192.168.5.8", RPort: 443, Weight: 1, Enabled: true},
			{RIP: "192.168.5.9", RPort: 443, Weight: 1, Enabled: true},
		},
	}
}

func masterDir() RenderDirector {
	return RenderDirector{
		State: "MASTER", Priority: 150, VirtualRouterID: 51,
		UnicastSrcIP: "192.168.5.6", UnicastPeerIP: "192.168.5.7",
		Interface: "eth0", Mode: "DR", VIP: "192.168.5.5", VRRPInstance: "VI_1",
	}
}

func backupDir() RenderDirector {
	d := masterDir()
	d.State = "BACKUP"
	d.Priority = 100
	d.UnicastSrcIP = "192.168.5.7"
	d.UnicastPeerIP = "192.168.5.6"
	return d
}

func TestRenderKeepalived_Master(t *testing.T) {
	out := RenderKeepalived(masterDir(), sampleVS(), sampleRS())
	want := []string{
		"vrrp_instance VI_1 {",
		"    state MASTER",
		"    priority 150",
		"    virtual_router_id 51",
		"    unicast_src_ip 192.168.5.6",
		"    unicast_peer {",
		"        192.168.5.7",
		"    }",
		"    virtual_ipaddress {",
		"        192.168.5.5/32 dev lo",
		"virtual_server 192.168.5.5 80 {",
		"    lb_algo wrr",
		"    lb_kind DR",
		"    protocol TCP",
		"    real_server 192.168.5.8 80 {",
		"        weight 1",
		"    real_server 192.168.5.9 80 {",
		"virtual_server 192.168.5.5 443 {",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("渲染结果缺少期望片段 %q\n--- 实际输出 ---\n%s", w, out)
		}
	}
}

func TestRenderKeepalived_MasterBackupOnlyThreeDiffs(t *testing.T) {
	master := RenderKeepalived(masterDir(), sampleVS(), sampleRS())
	backup := RenderKeepalived(backupDir(), sampleVS(), sampleRS())

	// 按行拆分，忽略 unicast_src_ip / unicast_peer 行后，其余应完全一致。
	mLines := strings.Split(master, "\n")
	bLines := strings.Split(backup, "\n")
	if len(mLines) != len(bLines) {
		t.Fatalf("主备行数不一致：master=%d backup=%d", len(mLines), len(bLines))
	}
	diffs := 0
	for i := range mLines {
		ml, bl := strings.TrimSpace(mLines[i]), strings.TrimSpace(bLines[i])
		if ml == bl {
			continue
		}
		// 允许的 3 处差异：state / priority / unicast_src_ip / unicast_peer 内容
		if strings.HasPrefix(ml, "state ") ||
			strings.HasPrefix(ml, "priority ") ||
			strings.HasPrefix(ml, "unicast_src_ip ") ||
			strings.HasPrefix(strings.TrimSpace(mLines[i]), "192.168.5.") && (strings.Contains(mLines[i], "unicast_peer") || isPeerLine(mLines[i], bLines[i])) {
			diffs++
			continue
		}
		t.Fatalf("出现预期外的主备差异（仅允许 state/priority/unicast 三处）：\n master: %s\n backup: %s", mLines[i], bLines[i])
	}
	// 至少应检出 state/priority/unicast_src_ip 三处差异
	if diffs < 3 {
		t.Fatalf("主备差异少于 3 处（期望 state/priority/unicast_src_ip），实际 %d", diffs)
	}
}

func isPeerLine(a, b string) bool {
	ta, tb := strings.TrimSpace(a), strings.TrimSpace(b)
	return (strings.Contains(ta, "192.168.5.6") || strings.Contains(ta, "192.168.5.7")) &&
		(strings.Contains(tb, "192.168.5.6") || strings.Contains(tb, "192.168.5.7"))
}

func TestCheckVirtualRouterIDConflicts(t *testing.T) {
	// 同 VRID 双机 → 冲突
	dup := CheckVirtualRouterIDConflicts([]RenderDirector{
		{VirtualRouterID: 51}, {VirtualRouterID: 51},
	})
	if len(dup) != 1 || dup[0] != 51 {
		t.Fatalf("期望检测到 VRID 51 冲突，实际 %v", dup)
	}
	// 不同 VRID → 无冲突
	ok := CheckVirtualRouterIDConflicts([]RenderDirector{
		{VirtualRouterID: 51}, {VirtualRouterID: 52},
	})
	if len(ok) != 0 {
		t.Fatalf("期望无冲突，实际 %v", ok)
	}
}

func TestRenderKeepalived_DisabledRSOmitted(t *testing.T) {
	rs := map[string][]RenderRS{
		"192.168.5.5:80": {
			{RIP: "192.168.5.8", RPort: 80, Weight: 1, Enabled: true},
			{RIP: "192.168.5.9", RPort: 80, Weight: 1, Enabled: false},
		},
	}
	out := RenderKeepalived(masterDir(), sampleVS(), rs)
	if !strings.Contains(out, "real_server 192.168.5.8 80") {
		t.Fatal("启用的 RS 应出现")
	}
	if strings.Contains(out, "real_server 192.168.5.9 80") {
		t.Fatal("禁用的 RS 不应出现")
	}
}
