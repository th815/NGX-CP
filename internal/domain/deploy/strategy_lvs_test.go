// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package deploy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/th/ngxcp/internal/agent/executor"
	"github.com/th/ngxcp/internal/domain/lvs"
)

// ── fakes ────────────────────────────────────────────────────────────────

type weightCall struct {
	VS lvs.VirtualServerRef
	RS lvs.RealServerRef
	W  int
}

type fakeSetter struct {
	vss   []lvs.VirtualServer
	calls []weightCall
}

func (f *fakeSetter) SetWeight(_ context.Context, vs lvs.VirtualServerRef, rs lvs.RealServerRef, w int) error {
	f.calls = append(f.calls, weightCall{VS: vs, RS: rs, W: w})
	return nil
}

func (f *fakeSetter) ListVirtualServers(context.Context) ([]lvs.VirtualServer, error) {
	return f.vss, nil
}

func countByWeight(calls []weightCall) map[int]int {
	m := map[int]int{}
	for _, c := range calls {
		m[c.W]++
	}
	return m
}

func fakeRunner(out string) func(context.Context, string, ...string) (string, error) {
	return func(context.Context, string, ...string) (string, error) { return out, nil }
}

const (
	validNginxT   = "nginx: the configuration file /etc/nginx/nginx.conf syntax is ok\nnginx: configuration file /etc/nginx/nginx.conf test is successful"
	invalidNginxT = "nginx: [emerg] unexpected end of file\nnginx: configuration file /etc/nginx/nginx.conf test failed"
)

// sampleVSS2 含两台 backend（192.168.5.8 / 192.168.5.9），各挂 80/443tcp/443udp。
func sampleVSS2() []lvs.VirtualServer {
	mk := func(proto string, port int) lvs.VirtualServer {
		return lvs.VirtualServer{
			Ref:       lvs.VirtualServerRef{Proto: proto, Address: "192.168.5.5", Port: port},
			Scheduler: "wrr",
			RealServers: []lvs.RealServer{
				{Ref: lvs.RealServerRef{Address: "192.168.5.8", Port: port}, Forward: "Route", Weight: 1},
				{Ref: lvs.RealServerRef{Address: "192.168.5.9", Port: port}, Forward: "Route", Weight: 1},
			},
		}
	}
	return []lvs.VirtualServer{mk("TCP", 80), mk("TCP", 443), mk("UDP", 443)}
}

// ── LVSStrategy 接线测试 ─────────────────────────────────────────────────

func TestLVSStrategy_DeployNodeCanary_EndToEnd(t *testing.T) {
	setter := &fakeSetter{vss: sampleVSS2()}
	deployExe := executor.NewDeployExecutorWithRunner(fakeRunner(validNginxT))
	strat := NewLVSStrategy(deployExe, setter /* 无探活配置：跳过真实 HTTP */)
	strat.SetTimings(50*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond)

	prefix := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(prefix, "conf.d"), 0o755))
	req := executor.DeployRequest{
		Files:         []executor.DeployFile{{Path: "conf.d/api.conf", Content: "server{}"}},
		Prefix:        prefix,
		ObserveWindow: 5 * time.Millisecond,
	}

	err := strat.DeployNodeCanary(context.Background(), 8, lvs.BackendRef{Address: "192.168.5.8"}, req)
	require.NoError(t, err)

	// 9 步落盘确实生效：文件落到 prefix
	got, err := os.ReadFile(filepath.Join(prefix, "conf.d/api.conf"))
	require.NoError(t, err)
	assert.Equal(t, "server{}", string(got))

	// 权重：3 条 VS 先 0 后还原 1（defer 兜底可能再幂等加回一次，故用 >=）
	cw := countByWeight(setter.calls)
	assert.Equal(t, 3, cw[0], "摘除应覆盖 80/443tcp/443udp")
	assert.GreaterOrEqual(t, cw[1], 3, "加回应覆盖 80/443tcp/443udp（defer 兜底可能重复，无害）")
}

func TestLVSStrategy_DeployNodeCanary_ChangeFailsRestoresWeight(t *testing.T) {
	setter := &fakeSetter{vss: sampleVSS2()}
	// 校验失败 → 9 步在切换前中止 → 灰度应返回错误，且权重已被加回
	deployExe := executor.NewDeployExecutorWithRunner(fakeRunner(invalidNginxT))
	strat := NewLVSStrategy(deployExe, setter)
	strat.SetTimings(50*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond)

	prefix := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(prefix, "conf.d"), 0o755))
	req := executor.DeployRequest{
		Files:         []executor.DeployFile{{Path: "conf.d/api.conf", Content: "broken{"}},
		Prefix:        prefix,
		ObserveWindow: 5 * time.Millisecond,
	}

	err := strat.DeployNodeCanary(context.Background(), 8, lvs.BackendRef{Address: "192.168.5.8"}, req)
	require.Error(t, err, "nginx -t 失败应使灰度返回错误")
	cw := countByWeight(setter.calls)
	assert.Equal(t, 3, cw[0])
	assert.Equal(t, 3, cw[1], "变更失败后权重必须加回，不能把节点留在池外")
}

func TestLVSStrategy_DeployAll_Sequential(t *testing.T) {
	setter := &fakeSetter{vss: sampleVSS2()}
	deployExe := executor.NewDeployExecutorWithRunner(fakeRunner(validNginxT))
	strat := NewLVSStrategy(deployExe, setter)
	strat.SetTimings(50*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond)

	prefix := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(prefix, "conf.d"), 0o755))
	req := executor.DeployRequest{
		Files:         []executor.DeployFile{{Path: "conf.d/api.conf", Content: "server{}"}},
		Prefix:        prefix,
		ObserveWindow: 5 * time.Millisecond,
	}

	// 先摘 .8 做完，再摘 .9；两台各自 3×0 + 3×1
	err := strat.DeployAll(context.Background(),
		[]int{8, 9},
		[]lvs.BackendRef{{Address: "192.168.5.8"}, {Address: "192.168.5.9"}},
		req)
	require.NoError(t, err)
	cw := countByWeight(setter.calls)
	assert.Equal(t, 6, cw[0], "两台 backend 各 3 条 VS 摘除")
	assert.GreaterOrEqual(t, cw[1], 6, "两台 backend 各 3 条 VS 加回（defer 兜底可能重复，无害）")
}
