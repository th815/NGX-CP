// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

// fakeRunner 模拟 nginx 命令：按首个参数返回脚本化输出。
// tMode="ok" → nginx -t 通过；"bad" → 语法错误；reload 始终成功。
func fakeRunner(mode string) func(ctx context.Context, name string, args ...string) (string, error) {
	return func(ctx context.Context, name string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "-t":
			if mode == "bad" {
				return "nginx: [emerg] unknown directive \"lstne\" in /etc/nginx/conf.d/bad.conf:1", nil
			}
			return "nginx: configuration file /etc/nginx/nginx.conf test is successful", nil
		case "-s":
			return "", nil
		}
		return "", nil
	}
}

// fakeProber 可控探活器。
type fakeProber struct{ ok bool }

func (f fakeProber) Probe(ctx context.Context) (bool, string, error) {
	if f.ok {
		return true, "HTTP 200", nil
	}
	return false, "HTTP 502", nil
}

func collectProgress(ch chan Progress) []Progress {
	var out []Progress
	for {
		select {
		case p := <-ch:
			out = append(out, p)
		default:
			return out
		}
	}
}

func TestDeploy_HappyPath_WritesFilesAndReports(t *testing.T) {
	prefix := t.TempDir()
	ex := NewDeployExecutorWithRunner(fakeRunner("ok"))
	ex.SetProber(fakeProber{ok: true}) // 探活走可控桩

	progress := make(chan Progress, 64)
	res, err := ex.Deploy(context.Background(), DeployRequest{
		Prefix:        prefix,
		ObserveWindow: 10 * time.Millisecond,
		Files:         []DeployFile{{Path: "nginx.conf", Content: "events{} http{}"}, {Path: "conf.d/api.conf", Content: "server{}"}},
		ProbeURL:      "http://127.0.0.1/healthz",
	}, progress)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.Restored)
	assert.NotEmpty(t, res.SnapshotPath)

	got, rerr := os.ReadFile(filepath.Join(prefix, "conf.d/api.conf"))
	require.NoError(t, rerr)
	assert.Equal(t, "server{}", string(got))

	steps := collectProgress(progress)
	assert.Equal(t, "success", lastStatus(steps, "transfer"))
	assert.Equal(t, "success", lastStatus(steps, "validate"))
	assert.Equal(t, "success", lastStatus(steps, "switch"))
	assert.Equal(t, "success", lastStatus(steps, "reload"))
	assert.Equal(t, "success", lastStatus(steps, "probe"))
}

func TestDeploy_ValidateFails_ZeroPollution(t *testing.T) {
	prefix := t.TempDir()
	// 预置一个现有文件，验证其不被触碰
	require.NoError(t, os.MkdirAll(filepath.Join(prefix, "conf.d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(prefix, "conf.d/old.conf"), []byte("old"), 0o644))

	ex := NewDeployExecutorWithRunner(fakeRunner("bad"))
	_, err := ex.Deploy(context.Background(), DeployRequest{
		Prefix: prefix,
		Files:  []DeployFile{{Path: "nginx.conf", Content: "events{} http{}"}, {Path: "conf.d/bad.conf", Content: "lstne 80;"}},
	}, nil)
	require.Error(t, err)
	assert.Equal(t, apperr.CodePrecondition, apperr.CodeOf(err), "校验失败应返回前置条件错误")

	// 新文件绝不应落盘（零污染）
	_, statErr := os.Stat(filepath.Join(prefix, "conf.d/bad.conf"))
	assert.True(t, os.IsNotExist(statErr), "语法错误的配置不应写入线上")
	// 旧文件不受影响
	old, rerr := os.ReadFile(filepath.Join(prefix, "conf.d/old.conf"))
	require.NoError(t, rerr)
	assert.Equal(t, "old", string(old))
}

func TestDeploy_ProbeFails_AutoRollback(t *testing.T) {
	prefix := t.TempDir()
	// 预置原始配置
	require.NoError(t, os.WriteFile(filepath.Join(prefix, "nginx.conf"), []byte("events{} http{}"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(prefix, "conf.d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(prefix, "conf.d/api.conf"), []byte("server{ old }"), 0o644))

	ex := NewDeployExecutorWithRunner(fakeRunner("ok"))
	ex.SetProber(fakeProber{ok: false}) // 探活必失败

	res, err := ex.Deploy(context.Background(), DeployRequest{
		Prefix:        prefix,
		ObserveWindow: 10 * time.Millisecond,
		Files:         []DeployFile{{Path: "nginx.conf", Content: "events{} http{}"}, {Path: "conf.d/api.conf", Content: "server{ new }"}},
		ProbeURL:      "http://127.0.0.1/healthz",
	}, nil)
	require.Error(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Restored, "探活失败应触发回滚")

	// 线上配置应被还原为原始内容
	got, rerr := os.ReadFile(filepath.Join(prefix, "conf.d/api.conf"))
	require.NoError(t, rerr)
	assert.Equal(t, "server{ old }", string(got), "回滚后应为变更前内容")
}

func TestDeploy_HashMismatch_AbortsBeforeSwitch(t *testing.T) {
	prefix := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(prefix, "conf.d"), 0o755))

	ex := NewDeployExecutorWithRunner(fakeRunner("ok"))
	_, err := ex.Deploy(context.Background(), DeployRequest{
		Prefix: prefix,
		Files:  []DeployFile{{Path: "conf.d/api.conf", Content: "server{}", SHA256: "deadbeef"}},
	}, nil)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(prefix, "conf.d/api.conf"))
	assert.True(t, os.IsNotExist(statErr), "摘要不符不应落盘")
}

func TestDeploy_NoProbeURL_SkipsProbe(t *testing.T) {
	prefix := t.TempDir()
	ex := NewDeployExecutorWithRunner(fakeRunner("ok"))
	res, err := ex.Deploy(context.Background(), DeployRequest{
		Prefix:        prefix,
		ObserveWindow: 10 * time.Millisecond,
		Files:         []DeployFile{{Path: "conf.d/api.conf", Content: "server{}"}},
	}, nil)
	require.NoError(t, err)
	assert.False(t, res.Restored)
}

// lastStatus 从进度序列里取某个步骤最后一次的状态。
func lastStatus(steps []Progress, step string) string {
	var s string
	for _, p := range steps {
		if p.Step == step {
			s = p.Status
		}
	}
	return s
}

// 确保 ObserveWindow 不影响快速测试（默认可被 0 覆盖场景）。
func TestDeploy_ObserveWindowWaits(t *testing.T) {
	prefix := t.TempDir()
	ex := NewDeployExecutorWithRunner(fakeRunner("ok"))
	start := time.Now()
	_, err := ex.Deploy(context.Background(), DeployRequest{
		Prefix:        prefix,
		ObserveWindow: 20 * time.Millisecond,
		Files:         []DeployFile{{Path: "conf.d/api.conf", Content: "server{}"}},
	}, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond, "应等待观测窗口")
}
