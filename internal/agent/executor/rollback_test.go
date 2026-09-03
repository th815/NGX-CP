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

// makeRollbackFixture 在沙盒内构造 prefix（sb/etc/nginx）+ 预置原始配置，并返回 prefix 与快照路径。
// 快照抓取后会把 prefix 内容改成 "CHANGED" 以模拟 deploy 失败态，调用方再触发回滚验证恢复。
func makeRollbackFixture(t *testing.T) (prefix, snapPath string) {
	t.Helper()
	sb := t.TempDir()
	prefix = filepath.Join(sb, "etc", "nginx")
	require.NoError(t, os.MkdirAll(filepath.Join(prefix, "conf.d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(prefix, "nginx.conf"), []byte("events{} http{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(prefix, "conf.d/api.conf"), []byte("server{ old }"), 0o644))

	snapper := NewSnapshotExecutor()
	co, err := snapper.Create(context.Background(), SnapshotRequest{
		Paths: []string{prefix}, StagingDir: t.TempDir(), NodeID: 1, Type: "pre_deploy",
	})
	require.NoError(t, err)
	require.NotEmpty(t, co.Path)
	snapPath = co.Path

	// 模拟 deploy 失败态：配置文件被改成新内容（尚未回滚）
	require.NoError(t, os.WriteFile(filepath.Join(prefix, "conf.d/api.conf"), []byte("server{ CHANGED }"), 0o644))
	return prefix, snapPath
}

func TestRollback_Success_RestoresFiles(t *testing.T) {
	prefix, snapPath := makeRollbackFixture(t)

	rb := NewRollbackExecutorWithRunner(fakeRunner("ok"))
	rb.SetProber(fakeProber{ok: true})

	res, err := rb.Rollback(context.Background(), RollbackRequest{
		SnapshotPath:  snapPath,
		Prefix:        prefix,
		RestoreRoot:   "/", // 默认恢复根：快照条目相对 /，拼回即为真实 prefix（测试 prefix 在 /tmp 下，恢复落到 prefix 本身）
		ObserveWindow: 10 * time.Millisecond,
		ProbeURL:      "http://127.0.0.1/healthz",
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.RolledBack)
	assert.Empty(t, res.Error, "成功回滚不应有 Error")

	got, rerr := os.ReadFile(filepath.Join(prefix, "conf.d/api.conf"))
	require.NoError(t, rerr)
	assert.Equal(t, "server{ old }", string(got), "回滚后应恢复为变更前内容")
	ng, nerr := os.ReadFile(filepath.Join(prefix, "nginx.conf"))
	require.NoError(t, nerr)
	assert.Equal(t, "events{} http{}", string(ng))
}

func TestRollback_SnapshotConfigBad_PrefixUntouched(t *testing.T) {
	prefix, snapPath := makeRollbackFixture(t)

	rb := NewRollbackExecutorWithRunner(fakeRunner("bad")) // nginx -t 失败 → 快照配置坏
	rb.SetProber(fakeProber{ok: true})

	res, err := rb.Rollback(context.Background(), RollbackRequest{
		SnapshotPath:  snapPath,
		Prefix:        prefix,
		RestoreRoot:   "/", // 默认恢复根：快照条目相对 /，拼回即为真实 prefix（测试 prefix 在 /tmp 下，恢复落到 prefix 本身）
		ObserveWindow: 10 * time.Millisecond,
	}, nil)
	require.Error(t, err)
	require.NotNil(t, res)
	assert.Contains(t, res.Error, "快照配置校验失败")
	assert.False(t, res.RolledBack, "快照配置坏时不应执行恢复，prefix 应保持 deploy 失败态")

	// prefix 不被改动（仍是 deploy 失败态）
	got, rerr := os.ReadFile(filepath.Join(prefix, "conf.d/api.conf"))
	require.NoError(t, rerr)
	assert.Equal(t, "server{ CHANGED }", string(got), "快照不可恢复时线上必须零变化")
}

func TestRollback_ProbeFails_AfterRestore_ReportedFailed(t *testing.T) {
	prefix, snapPath := makeRollbackFixture(t)

	rb := NewRollbackExecutorWithRunner(fakeRunner("ok"))
	rb.SetProber(fakeProber{ok: false}) // 恢复成功但探活失败

	res, err := rb.Rollback(context.Background(), RollbackRequest{
		SnapshotPath:  snapPath,
		Prefix:        prefix,
		RestoreRoot:   "/", // 默认恢复根：快照条目相对 /，拼回即为真实 prefix（测试 prefix 在 /tmp 下，恢复落到 prefix 本身）
		ObserveWindow: 10 * time.Millisecond,
		ProbeURL:      "http://127.0.0.1/healthz",
	}, nil)
	require.Error(t, err)
	require.NotNil(t, res)
	assert.Contains(t, res.Error, "回滚后探活失败")
	assert.True(t, res.RolledBack, "恢复已生效，但探活不过 → rollback_failed")

	// 文件已恢复（恢复在探活之前）
	got, rerr := os.ReadFile(filepath.Join(prefix, "conf.d/api.conf"))
	require.NoError(t, rerr)
	assert.Equal(t, "server{ old }", string(got))
}

func TestRollback_NoProbeURL_SkipsProbe(t *testing.T) {
	prefix, snapPath := makeRollbackFixture(t)

	rb := NewRollbackExecutorWithRunner(fakeRunner("ok")) // 不调 SetProber，ProbeURL 空 → 跳过

	res, err := rb.Rollback(context.Background(), RollbackRequest{
		SnapshotPath:  snapPath,
		Prefix:        prefix,
		RestoreRoot:   "/", // 默认恢复根：快照条目相对 /，拼回即为真实 prefix（测试 prefix 在 /tmp 下，恢复落到 prefix 本身）
		ObserveWindow: 10 * time.Millisecond,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.RolledBack)
	assert.Empty(t, res.Error)

	got, rerr := os.ReadFile(filepath.Join(prefix, "conf.d/api.conf"))
	require.NoError(t, rerr)
	assert.Equal(t, "server{ old }", string(got))
}

func TestRollback_MissingSnapshotPath_Fails(t *testing.T) {
	prefix, _ := makeRollbackFixture(t)
	rb := NewRollbackExecutorWithRunner(fakeRunner("ok"))
	_, err := rb.Rollback(context.Background(), RollbackRequest{
		Prefix: prefix, RestoreRoot: "/",
	}, nil)
	require.Error(t, err)
	assert.Equal(t, apperr.CodeInvalid, apperr.CodeOf(err))
}
