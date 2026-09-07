// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package lvs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	domainlvs "github.com/th/ngxcp/internal/domain/lvs"
	"github.com/th/ngxcp/ent"
)

// fakeSetter 是 domainlvs.WeightSetter 的内存实现，记录每次调用。
type fakeSetter struct {
	calls []fakeCall
}

type fakeCall struct {
	vs     domainlvs.VirtualServerRef
	rs     domainlvs.RealServerRef
	weight int
}

func (f *fakeSetter) SetWeight(_ context.Context, vs domainlvs.VirtualServerRef, rs domainlvs.RealServerRef, w int) error {
	f.calls = append(f.calls, fakeCall{vs: vs, rs: rs, weight: w})
	return nil
}

func (f *fakeSetter) ListVirtualServers(_ context.Context) ([]domainlvs.VirtualServer, error) {
	return nil, nil
}

// seedDrain 构造一个 RS 节点 + Director 节点 + VS(:80/:443) + 两条 RS 记录（同一 rip），返回 svc 与第一条 RS 的 id。
func seedDrain(t *testing.T, c *ent.Client) (*Service, int) {
	t.Helper()
	ctx := context.Background()
	rsNode, err := c.Node.Create().SetName("rs-01").SetAddress("192.168.5.8").
		SetRole("real_server").SetStatus("online").Save(ctx)
	require.NoError(t, err)
	dirNode, err := c.Node.Create().SetName("dir-01").SetAddress("192.168.5.6").
		SetRole("director").SetStatus("online").Save(ctx)
	require.NoError(t, err)
	d, err := c.Director.Create().SetState("MASTER").SetPriority(150).SetVirtualRouterID(51).
		SetUnicastSrcIP("192.168.5.6").SetUnicastPeerIP("192.168.5.7").SetIface("eth0").
		SetMode("DR").SetVip("192.168.5.5").SetNode(dirNode).Save(ctx)
	require.NoError(t, err)
	_, err = c.VirtualService.Create().SetVip("192.168.5.5").SetPort(80).
		SetProtocol("tcp").SetScheduler("wrr").SetDirector(d).Save(ctx)
	require.NoError(t, err)
	_, err = c.VirtualService.Create().SetVip("192.168.5.5").SetPort(443).
		SetProtocol("tcp").SetScheduler("wrr").SetDirector(d).Save(ctx)
	require.NoError(t, err)
	r80, err := c.RealServer.Create().SetVip("192.168.5.5").SetVport(80).
		SetRip("192.168.5.8").SetRport(80).SetWeight(1).SetEnabled(true).SetNode(rsNode).Save(ctx)
	require.NoError(t, err)
	_, err = c.RealServer.Create().SetVip("192.168.5.5").SetVport(443).
		SetRip("192.168.5.8").SetRport(443).SetWeight(1).SetEnabled(true).SetNode(rsNode).Save(ctx)
	require.NoError(t, err)
	return New(c), r80.ID
}

func TestService_Drain(t *testing.T) {
	c := openTest(t)
	defer c.Close()
	fs := &fakeSetter{}
	svc, rsID := seedDrain(t, c)
	svc.SetWeightSetter(fs)

	require.NoError(t, svc.Drain(context.Background(), rsID))
	// 同一 rip 关联 :80 与 :443 两条，必须全部置 0。
	require.Len(t, fs.calls, 2)
	for _, call := range fs.calls {
		require.Equal(t, 0, call.weight)
	}
	// 模型：Enabled=false，Weight 基线保留为 1。
	recs, err := c.RealServer.Query().All(context.Background())
	require.NoError(t, err)
	require.Len(t, recs, 2)
	for _, r := range recs {
		require.False(t, r.Enabled)
		require.Equal(t, 1, r.Weight)
	}
}

func TestService_Restore(t *testing.T) {
	c := openTest(t)
	defer c.Close()
	fs := &fakeSetter{}
	svc, rsID := seedDrain(t, c)
	svc.SetWeightSetter(fs)

	require.NoError(t, svc.Drain(context.Background(), rsID))
	require.NoError(t, svc.Restore(context.Background(), rsID))
	// 摘除 2 条 + 恢复 2 条 = 4 次下发；恢复下发基线权重 1。
	require.Len(t, fs.calls, 4)
	for _, call := range fs.calls[2:] {
		require.Equal(t, 1, call.weight)
	}
	recs, err := c.RealServer.Query().All(context.Background())
	require.NoError(t, err)
	for _, r := range recs {
		require.True(t, r.Enabled)
		require.Equal(t, 1, r.Weight)
	}
}

func TestService_SetBaselineWeight(t *testing.T) {
	c := openTest(t)
	defer c.Close()
	fs := &fakeSetter{}
	svc, rsID := seedDrain(t, c)
	svc.SetWeightSetter(fs)

	require.NoError(t, svc.SetBaselineWeight(context.Background(), rsID, 3))
	require.Len(t, fs.calls, 2)
	for _, call := range fs.calls {
		require.Equal(t, 3, call.weight)
	}
	recs, err := c.RealServer.Query().All(context.Background())
	require.NoError(t, err)
	for _, r := range recs {
		require.True(t, r.Enabled)
		require.Equal(t, 3, r.Weight)
	}
}

func TestService_Drain_BlockedByGate(t *testing.T) {
	c := openTest(t)
	defer c.Close()
	fs := &fakeSetter{}
	svc, rsID := seedDrain(t, c)
	svc.SetWeightSetter(fs)
	// 门禁：该 RS 节点（rsNode）状态判定为不通过。
	svc.SetGate(NewGate(func(_ context.Context, _ int) (bool, []string, error) {
		return false, []string{"vip_on_lo"}, nil
	}))

	err := svc.Drain(context.Background(), rsID)
	require.Error(t, err)
	// 门禁阻断：Agent 通道不应被调用。
	require.Empty(t, fs.calls)
}
