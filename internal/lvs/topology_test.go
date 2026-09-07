package lvs

import (
	"context"
	"testing"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/director"
	"github.com/th/ngxcp/ent/node"
	"github.com/th/ngxcp/ent/realserver"
	"github.com/th/ngxcp/ent/virtualservice"
	"github.com/th/ngxcp/internal/repo"
)

func openTest(t *testing.T) *ent.Client {
	t.Helper()
	c, err := repo.Open("sqlite", "file:lvstest?mode=memory&_fk=1")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := c.Schema.Create(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return c
}

func TestService_Topology(t *testing.T) {
	ctx := context.Background()
	c := openTest(t)
	defer c.Close()

	n, err := c.Node.Create().
		SetName("dir-01").
		SetAddress("192.168.5.6").
		SetRole("director").
		SetStatus("online").
		Save(ctx)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	d, err := c.Director.Create().
		SetState("MASTER").
		SetPriority(150).
		SetVirtualRouterID(51).
		SetUnicastSrcIP("192.168.5.6").
		SetUnicastPeerIP("192.168.5.7").
		SetIface("eth0").
		SetMode("DR").
		SetVip("192.168.5.5").
		SetNode(n).
		Save(ctx)
	if err != nil {
		t.Fatalf("create director: %v", err)
	}
	if _, err = c.VirtualService.Create().
		SetVip("192.168.5.5").
		SetPort(80).
		SetProtocol("tcp").
		SetScheduler("wrr").
		SetDirector(d).
		Save(ctx); err != nil {
		t.Fatalf("create vs: %v", err)
	}
	if _, err = c.RealServer.Create().
		SetVip("192.168.5.5").
		SetVport(80).
		SetRip("192.168.5.8").
		SetRport(80).
		SetWeight(1).
		SetNode(n).
		Save(ctx); err != nil {
		t.Fatalf("create rs: %v", err)
	}

	svc := New(c)
	topo, err := svc.Topology(ctx)
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if topo.VIP != "192.168.5.5" {
		t.Fatalf("VIP 期望 192.168.5.5，实际 %q", topo.VIP)
	}
	if len(topo.Directors) != 1 {
		t.Fatalf("期望 1 个 Director，实际 %d", len(topo.Directors))
	}
	dn := topo.Directors[0]
	if dn.State != "MASTER" || dn.Address != "192.168.5.6" || dn.Status != "online" {
		t.Fatalf("Director 视图异常: %+v", dn)
	}
	// 未上报 holding_vip 时按 MASTER 推断为持有
	if !dn.HoldingVIP {
		t.Fatalf("MASTER 应推断为持有 VIP")
	}
	if len(topo.RS) != 1 {
		t.Fatalf("期望 1 个 RS，实际 %d", len(topo.RS))
	}
	if topo.RS[0].VSKey != "192.168.5.5:80" || topo.RS[0].RIP != "192.168.5.8" {
		t.Fatalf("RS 视图异常: %+v", topo.RS[0])
	}

	_ = director.StateMASTER
	_ = virtualservice.SchedulerWrr
	_ = realserver.StateActive
	_ = node.RoleDirector
}

func TestService_VirtualServices(t *testing.T) {
	ctx := context.Background()
	c := openTest(t)
	defer c.Close()

	n, _ := c.Node.Create().SetName("d").SetAddress("192.168.5.6").SetRole("director").SetStatus("online").Save(ctx)
	d, _ := c.Director.Create().SetState("BACKUP").SetPriority(100).SetVirtualRouterID(51).
		SetUnicastSrcIP("192.168.5.7").SetUnicastPeerIP("192.168.5.6").SetIface("eth0").
		SetMode("DR").SetVip("192.168.5.5").SetNode(n).Save(ctx)
	if _, err := c.VirtualService.Create().SetVip("192.168.5.5").SetPort(443).
		SetProtocol("tcp").SetScheduler("wrr").SetDirector(d).Save(ctx); err != nil {
		t.Fatalf("create vs: %v", err)
	}

	svc := New(c)
	vs, err := svc.VirtualServices(ctx)
	if err != nil {
		t.Fatalf("VirtualServices: %v", err)
	}
	if len(vs) != 1 || vs[0].Port != 443 || vs[0].DirectorState != "BACKUP" {
		t.Fatalf("VS 视图异常: %+v", vs)
	}
}
