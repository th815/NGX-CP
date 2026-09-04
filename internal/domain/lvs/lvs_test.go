// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package lvs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/th/ngxcp/internal/agent/probe"
)

// ── fakes ───────────────────────────────────────────────────────────────

type weightCall struct {
	VS VirtualServerRef
	RS RealServerRef
	W  int
}

type fakeSetter struct {
	vss   []VirtualServer
	calls []weightCall
}

func (f *fakeSetter) SetWeight(_ context.Context, vs VirtualServerRef, rs RealServerRef, w int) error {
	f.calls = append(f.calls, weightCall{VS: vs, RS: rs, W: w})
	return nil
}

func (f *fakeSetter) ListVirtualServers(context.Context) ([]VirtualServer, error) {
	return f.vss, nil
}

type fakeProber struct{ ok bool }

func (f fakeProber) Probe(context.Context) (*probe.ProbeResult, error) {
	return &probe.ProbeResult{OK: f.ok}, nil
}

// sampleVSS 模拟生产环境的真实拓扑：一台 backend(192.0.2.8) 挂在 80/443(tcp)/443(udp) 三条 VS。
func sampleVSS() []VirtualServer {
	return []VirtualServer{
		{Ref: VirtualServerRef{Proto: "TCP", Address: "192.0.2.5", Port: 80}, Scheduler: "wrr",
			RealServers: []RealServer{{Ref: RealServerRef{Address: "192.0.2.8", Port: 80}, Forward: "Route", Weight: 1}}},
		{Ref: VirtualServerRef{Proto: "TCP", Address: "192.0.2.5", Port: 443}, Scheduler: "wrr",
			RealServers: []RealServer{{Ref: RealServerRef{Address: "192.0.2.8", Port: 443}, Forward: "Route", Weight: 1}}},
		{Ref: VirtualServerRef{Proto: "UDP", Address: "192.0.2.5", Port: 443}, Scheduler: "wrr",
			RealServers: []RealServer{{Ref: RealServerRef{Address: "192.0.2.8", Port: 443}, Forward: "Route", Weight: 1}}},
	}
}

func countByWeight(calls []weightCall) map[int]int {
	m := map[int]int{}
	for _, c := range calls {
		m[c.W]++
	}
	return m
}

func distinctVS(calls []weightCall) map[string]int {
	m := map[string]int{}
	for _, c := range calls {
		m[c.VS.Key()]++
	}
	return m
}

// ── ParseIPVS ───────────────────────────────────────────────────────────

func TestParseIPVS_RealOutput(t *testing.T) {
	raw := `IP Virtual Server version 1.2.1 (size=4096)
Prot LocalAddress:Port Scheduler Flags
  -> RemoteAddress:Port           Forward Weight ActiveConn InActConn
TCP  192.0.2.5:80 wrr persistent 60
  -> 192.0.2.8:80               Route   1      0          0
  -> 192.0.2.9:80               Route   1      0          0
TCP  192.0.2.5:443 wrr persistent 60
  -> 192.0.2.8:443              Route   1      1          0
  -> 192.0.2.9:443             Route   1      0          0
UDP  192.0.2.5:443 wrr persistent 60
  -> 192.0.2.8:443             Route   1      0          0
  -> 192.0.2.9:443             Route   1      0          0`
	vss, err := ParseIPVS(raw)
	require.NoError(t, err)
	require.Len(t, vss, 3)
	assert.Equal(t, "TCP", vss[0].Ref.Proto)
	assert.Equal(t, 443, vss[1].Ref.Port)
	assert.Equal(t, "UDP", vss[2].Ref.Proto)
	// 每台 VS 2 个 RS
	for _, vs := range vss {
		require.Len(t, vs.RealServers, 2)
	}
	// 权重 / ActiveConn 解析正确
	assert.Equal(t, 1, vss[1].RealServers[0].Weight)
	assert.Equal(t, 1, vss[1].RealServers[0].ActiveConn)
	assert.Equal(t, "persistent 60", vss[2].Flags)
}

// ── GracefulDeploy ──────────────────────────────────────────────────────

func newGraceful(setter *fakeSetter, deployer NodeDeployer, probers []probe.Prober) *GracefulDeploy {
	return &GracefulDeploy{
		Setter:        setter,
		Deployer:      deployer,
		Probers:       probers,
		DrainTimeout:  50 * time.Millisecond,
		ObserveWindow: 10 * time.Millisecond,
		DrainPoll:     5 * time.Millisecond,
	}
}

func TestGracefulDeploy_Happy_ZeroesThenRestoresAllVS(t *testing.T) {
	setter := &fakeSetter{vss: sampleVSS()}
	var deployed int
	gd := newGraceful(setter, func(context.Context, int) error { deployed++; return nil },
		[]probe.Prober{fakeProber{ok: true}})

	err := gd.DeployOne(context.Background(), 8, BackendRef{Address: "192.0.2.8"})
	require.NoError(t, err)
	assert.Equal(t, 1, deployed)

	// 权重操作必须覆盖全部 3 条 VS（80/443tcp/443udp），且先 0 后还原 1
	cw := countByWeight(setter.calls)
	assert.Equal(t, 3, cw[0], "摘除时应把 3 条 VS 的权重都置 0")
	// 步骤⑥ 已加回一次；函数返回时 defer 安全网会再幂等加回一次，故 1 至少出现 3 次
	assert.GreaterOrEqual(t, cw[1], 3, "加回时应把 3 条 VS 的权重都还原为 1（defer 兜底可能重复，无害）")
	assert.Len(t, distinctVS(setter.calls), 3, "应触及 3 个不同的 VS")
}

func TestGracefulDeploy_DeployFails_StillRestoresWeight(t *testing.T) {
	setter := &fakeSetter{vss: sampleVSS()}
	gd := newGraceful(setter, func(context.Context, int) error { return assert.AnError },
		[]probe.Prober{fakeProber{ok: true}})

	err := gd.DeployOne(context.Background(), 8, BackendRef{Address: "192.0.2.8"})
	require.Error(t, err, "变更失败应返回错误")
	// 即使变更失败，defer 也必须把权重加回原值（绝不能把节点留在池外）
	cw := countByWeight(setter.calls)
	assert.Equal(t, 3, cw[0])
	assert.Equal(t, 3, cw[1], "变更失败后权重必须被加回")
}

func TestGracefulDeploy_DrainTimeout_RestoresWeight(t *testing.T) {
	// ActiveConn 恒不为 0 → 排空必然超时
	stuck := sampleVSS()
	for i := range stuck {
		for j := range stuck[i].RealServers {
			stuck[i].RealServers[j].ActiveConn = 9
		}
	}
	setter := &fakeSetter{vss: stuck}
	gd := newGraceful(setter, func(context.Context, int) error { return nil },
		[]probe.Prober{fakeProber{ok: true}})

	err := gd.DeployOne(context.Background(), 8, BackendRef{Address: "192.0.2.8"})
	require.Error(t, err, "排空超时必须返回错误")
	cw := countByWeight(setter.calls)
	assert.Equal(t, 3, cw[0])
	assert.Equal(t, 3, cw[1], "排空超时后权重必须被加回")
}

func TestGracefulDeploy_ProbeFails_RestoresWeight(t *testing.T) {
	setter := &fakeSetter{vss: sampleVSS()}
	var deployed int
	gd := newGraceful(setter, func(context.Context, int) error { deployed++; return nil },
		[]probe.Prober{fakeProber{ok: false}})

	err := gd.DeployOne(context.Background(), 8, BackendRef{Address: "192.0.2.8"})
	require.Error(t, err, "探活失败必须返回错误")
	assert.Equal(t, 1, deployed, "变更应已执行")
	cw := countByWeight(setter.calls)
	assert.Equal(t, 3, cw[0])
	assert.Equal(t, 3, cw[1], "探活失败后权重必须被加回")
}

func TestGracefulDeploy_BackendNotInVS_Precondition(t *testing.T) {
	setter := &fakeSetter{vss: sampleVSS()}
	gd := newGraceful(setter, func(context.Context, int) error { return nil }, nil)
	err := gd.DeployOne(context.Background(), 9, BackendRef{Address: "10.0.0.99"})
	require.Error(t, err)
	assert.Equal(t, 0, len(setter.calls), "backend 不存在时不应有任何权重操作")
}
