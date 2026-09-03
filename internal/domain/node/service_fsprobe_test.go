package node

import (
	"context"
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/ent"
	entnode "github.com/th/ngxcp/ent/node"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/repo"
)

// newDBNode 起一个内存 sqlite + 自动建表，并创建一个 online 态节点，返回 client 与节点 ID。
func newDBNode(t *testing.T, name string) (*ent.Client, int) {
	t.Helper()
	client, err := repo.Open("sqlite", "file:"+name+"?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("建表: %v", err)
	}
	n, err := client.Node.Create().
		SetName(name).
		SetAddress("10.0.0.1").
		SetRole(entnode.RoleRealServer).
		SetStatus(entnode.StatusOnline).
		Save(context.Background())
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	return client, n.ID
}

// TestFsProbeDegradesAndRecovers 验证 T018：FS 探测关键项失败 → degraded；恢复 → online。
func TestFsProbeDegradesAndRecovers(t *testing.T) {
	client, id := newDBNode(t, "fsprobe")
	defer client.Close()
	svc := New(client)

	fail := &agentv1.FsProbeReport{CheckedAt: 1, Items: []*agentv1.ComplianceItem{
		{Name: "disk_usage_nginx_paths", Severity: "critical", Passed: false, Actual: "92%"},
	}}
	if err := svc.SetFsProbe(context.Background(), id, fail); err != nil {
		t.Fatalf("SetFsProbe: %v", err)
	}
	if st := statusOf(t, client, id); st != entnode.StatusDegraded {
		t.Fatalf("FS 探测失败后状态 = %q, want degraded", st)
	}

	ok := &agentv1.FsProbeReport{CheckedAt: 2, Items: []*agentv1.ComplianceItem{
		{Name: "disk_usage_nginx_paths", Severity: "critical", Passed: true},
	}}
	if err := svc.SetFsProbe(context.Background(), id, ok); err != nil {
		t.Fatalf("SetFsProbe(恢复): %v", err)
	}
	if st := statusOf(t, client, id); st != entnode.StatusOnline {
		t.Fatalf("FS 探测恢复后状态 = %q, want online", st)
	}
}

// TestHealthAggregationCrossDimension 验证 degraded 由「合规 + FS」两维度聚合决定，
// 修复 T019 的独立翻转竞态：合规判 degraded 后，单条通过的 FS 报告不得错误翻回 online；
// 必须两维度都恢复才回到 online。
func TestHealthAggregationCrossDimension(t *testing.T) {
	client, id := newDBNode(t, "agg")
	defer client.Close()
	svc := New(client)

	compFail := &agentv1.ComplianceReport{CheckedAt: 1, Items: []*agentv1.ComplianceItem{
		{Name: "vip_on_lo", Severity: "critical", Passed: false},
	}}
	if err := svc.SetCompliance(context.Background(), id, compFail); err != nil {
		t.Fatalf("SetCompliance: %v", err)
	}
	if st := statusOf(t, client, id); st != entnode.StatusDegraded {
		t.Fatalf("合规失败后状态 = %q, want degraded", st)
	}

	// FS 探测通过，但合规仍失败 → 必须保持 degraded（不得独立翻回 online）。
	fsOK := &agentv1.FsProbeReport{CheckedAt: 2, Items: []*agentv1.ComplianceItem{
		{Name: "disk_usage_nginx_paths", Severity: "critical", Passed: true},
	}}
	if err := svc.SetFsProbe(context.Background(), id, fsOK); err != nil {
		t.Fatalf("SetFsProbe: %v", err)
	}
	if st := statusOf(t, client, id); st != entnode.StatusDegraded {
		t.Fatalf("仅 FS 恢复、合规仍失败时状态 = %q, want degraded（聚合判定）", st)
	}

	// 合规也恢复 → 两维度都通过 → online。
	compOK := &agentv1.ComplianceReport{CheckedAt: 3, Items: []*agentv1.ComplianceItem{
		{Name: "vip_on_lo", Severity: "critical", Passed: true},
	}}
	if err := svc.SetCompliance(context.Background(), id, compOK); err != nil {
		t.Fatalf("SetCompliance(恢复): %v", err)
	}
	if st := statusOf(t, client, id); st != entnode.StatusOnline {
		t.Fatalf("两维度均恢复后状态 = %q, want online", st)
	}
}

// TestFsProbeNilIgnored 验证 nil 报告不驱动任何流转（与合规同策略）。
func TestFsProbeNilIgnored(t *testing.T) {
	client, id := newDBNode(t, "fsnil")
	defer client.Close()
	svc := New(client)
	if err := svc.SetFsProbe(context.Background(), id, nil); err != nil {
		t.Fatalf("SetFsProbe(nil): %v", err)
	}
	if st := statusOf(t, client, id); st != entnode.StatusOnline {
		t.Fatalf("nil 报告后状态 = %q, want online", st)
	}
	if rep, err := svc.GetFsProbe(context.Background(), id); err != nil || rep != nil {
		t.Fatalf("GetFsProbe = (%v, %v), want (nil, nil)", rep, err)
	}
	// 不存在节点应返回 CodeNotFound。
	if err := svc.SetFsProbe(context.Background(), 99999, &agentv1.FsProbeReport{}); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("未知节点错误 = %v, want CodeNotFound", err)
	}
}

func statusOf(t *testing.T, client *ent.Client, id int) entnode.Status {
	t.Helper()
	n, err := client.Node.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("查询节点: %v", err)
	}
	return n.Status
}
