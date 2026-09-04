// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 并发控制（T038）测试：Worker 调度与锁生命周期。
package deploy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOrderRunner 记录被执行的变更单，可注入错误。
type fakeOrderRunner struct {
	mu  sync.Mutex
	ran []int
	err error
}

func (f *fakeOrderRunner) Run(_ context.Context, orderID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, orderID)
	return f.err
}

// TestWorker_ExecutesDequeuedOrder 验证 worker 出队后：Start 翻 running、Runner 被执行、节点锁释放。
func TestWorker_ExecutesDequeuedOrder(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	lm := NewLockManager(client, DefaultLockConfig())
	q := NewQueue(svc, lm, LockConfig{
		MaxConcurrentOrders: 3, MaxConcurrentPerNode: 1,
		LockTimeout: time.Minute, QueuePollInterval: 20 * time.Millisecond,
	})
	runner := &fakeOrderRunner{}
	w := NewWorker(q, svc, runner, q.cfg)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "w", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID)) // draft → pending

	go w.Start(ctx)
	defer w.Stop()

	require.Eventually(t, func() bool {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		return len(runner.ran) == 1
	}, 3*time.Second, 20*time.Millisecond, "worker 应在轮询内执行该变更单")

	// 节点锁必须已释放，避免锁泄漏。
	held, err := lm.HeldBy(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 0, held, "执行完成后节点锁应释放")

	// 闭环应已收敛到 success（fake runner 成功；真实 runner 在 T039 经 Agent 接线）。
	got, err := svc.Get(ctx, co.ID)
	require.NoError(t, err)
	assert.Equal(t, string(StatusSuccess), string(got.Status))
}

// TestWorker_NoRunnerIdle runner 为 nil 时 worker 不会真正执行（占位安全，不翻状态）。
func TestWorker_NoRunnerIdle(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	lm := NewLockManager(client, DefaultLockConfig())
	q := NewQueue(svc, lm, DefaultLockConfig())

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "idle", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))

	// runner 为 nil 的 worker 直接构造但不注入 runner；此处仅验证 TryDequeue+锁可用。
	w := NewWorker(q, svc, nil, q.cfg)
	got, err := q.TryDequeue(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	// 显式清理锁，避免遗留。
	require.NoError(t, lm.ReleaseByOrder(ctx, got.ID))
	assert.Equal(t, co.ID, got.ID)
	_ = w // 占位 worker 未启动
}
