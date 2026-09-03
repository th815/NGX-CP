// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package deploy

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/repo"
)

// newDeployClient 打开一个 sqlite 库并自动建表。
// file=true 时使用临时文件（并发写入更稳，避免共享内存库的锁竞争）。
func newDeployClient(t *testing.T, file bool) (*ent.Client, string) {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared&_fk=1"
	if file {
		dsn = "file:" + filepath.Join(t.TempDir(), "deploy.db") + "?_fk=1&_busy_timeout=5000"
	}
	client, err := repo.Open("sqlite", dsn)
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(context.Background()))
	t.Cleanup(func() { client.Close() })
	return client, dsn
}

// TestTransition_Legal：合法转换成功并落库。
func TestTransition_Legal(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, false)
	svc := New(client)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "t1", Type: "config"})
	require.NoError(t, err)

	require.NoError(t, svc.Transition(ctx, co.ID, "draft", "pending_approval"))
	got, err := svc.Get(ctx, co.ID)
	require.NoError(t, err)
	assert.Equal(t, string(StatusPendingApproval), string(got.Status))
}

// TestTransition_IllegalRejected：非法迁移（success → running）被拒绝，不落库。
func TestTransition_IllegalRejected(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, false)
	svc := New(client)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "t2", Type: "config"})
	require.NoError(t, err)

	// 即便当前库里是 draft，success→running 不在状态机里，应直接返回非法。
	err = svc.Transition(ctx, co.ID, "success", "running")
	require.Error(t, err)
	assert.Equal(t, apperr.CodeInvalid, apperr.CodeOf(err))

	got, err := svc.Get(ctx, co.ID)
	require.NoError(t, err)
	assert.Equal(t, string(StatusDraft), string(got.Status), "非法迁移不应改变状态")
}

// TestTransition_OptimisticLock：并发转换只有一个成功（乐观锁）。
// 用 -race 运行以暴露数据竞争。
func TestTransition_OptimisticLock(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true) // 文件库，并发更稳
	svc := New(client)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "race", Type: "config"})
	require.NoError(t, err)

	var mu sync.Mutex
	var okCount, conflictCount int
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := svc.Transition(ctx, co.ID, "draft", "pending_approval")
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				okCount++
			} else if apperr.CodeOf(e) == apperr.CodeConflict {
				conflictCount++
			} else {
				t.Errorf("意外的错误：%v", e)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, okCount, "恰好一个转换应成功")
	assert.Equal(t, 1, conflictCount, "另一个应因乐观锁冲突失败")

	got, err := svc.Get(ctx, co.ID)
	require.NoError(t, err)
	assert.Equal(t, string(StatusPendingApproval), string(got.Status))
}

// TestTransition_RecoveryAfterRestart：running 状态持久化，
// 控制面"重启"（新建 client + Service 指向同一库）后仍能恢复。
func TestTransition_RecoveryAfterRestart(t *testing.T) {
	ctx := context.Background()
	client, dsn := newDeployClient(t, true)
	svc := New(client)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "recover", Type: "config"})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))
	require.NoError(t, svc.Approve(ctx, co.ID, "admin"))
	require.NoError(t, svc.Start(ctx, co.ID)) // → running

	// 模拟控制面重启：新 client + 新 Service，指向同一数据库文件。
	restarted, err := repo.Open("sqlite", dsn)
	require.NoError(t, err)
	defer restarted.Close()
	svc2 := New(restarted)

	running, err := svc2.ListByStatus(ctx, string(StatusRunning))
	require.NoError(t, err)
	require.Len(t, running, 1, "重启后应能恢复 running 的变更单")
	assert.Equal(t, co.ID, running[0].ID)
}
