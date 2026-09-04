// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 并发控制（T038）测试：节点锁互斥 / 过期复用 / 释放 / 队列串行 / 全局并行守卫。
package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLockManager_AcquireMutex 同一节点同时只能被一个变更单占用。
func TestLockManager_AcquireMutex(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	lm := NewLockManager(client, DefaultLockConfig())

	ok1, err := lm.Acquire(ctx, 1, 100, time.Minute)
	require.NoError(t, err)
	assert.True(t, ok1)

	ok2, err := lm.Acquire(ctx, 1, 200, time.Minute)
	require.NoError(t, err)
	assert.False(t, ok2, "同节点被占用，第二单应失败")

	held, err := lm.HeldBy(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 100, held)
}

// TestLockManager_AcquireExpiredReusable 过期锁被清理后同节点可重新抢占（防死锁）。
func TestLockManager_AcquireExpiredReusable(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	lm := NewLockManager(client, DefaultLockConfig())

	ok, err := lm.Acquire(ctx, 1, 100, 50*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)

	time.Sleep(80 * time.Millisecond)
	n, err := lm.CleanExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	ok2, err := lm.Acquire(ctx, 1, 200, time.Minute)
	require.NoError(t, err)
	assert.True(t, ok2, "过期清理后同节点应可再抢")
}

// TestLockManager_Release 释放后节点可被其他变更单占用；释放非持有者无副作用。
func TestLockManager_Release(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	lm := NewLockManager(client, DefaultLockConfig())

	ok, err := lm.Acquire(ctx, 1, 100, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, lm.Release(ctx, 1, 100))
	ok2, err := lm.Acquire(ctx, 1, 200, time.Minute)
	require.NoError(t, err)
	require.True(t, ok2)

	require.NoError(t, lm.Release(ctx, 1, 999)) // 非持有者，应无影响
	held, err := lm.HeldBy(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 200, held)
}

// TestQueue_TryDequeueSameNodeSerial 同一节点的两个 pending 单，只有一把能抢到锁。
func TestQueue_TryDequeueSameNodeSerial(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	lm := NewLockManager(client, DefaultLockConfig())
	q := NewQueue(svc, lm, DefaultLockConfig())

	a, err := svc.CreateDraft(ctx, CreateInput{Title: "a", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)
	b, err := svc.CreateDraft(ctx, CreateInput{Title: "b", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, a.ID))
	require.NoError(t, svc.Submit(ctx, b.ID))

	first, err := q.TryDequeue(ctx)
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := q.TryDequeue(ctx)
	require.NoError(t, err)
	assert.Nil(t, second, "节点 1 已被锁，第二单应取不到")

	require.NoError(t, lm.ReleaseByOrder(ctx, first.ID))
	third, err := q.TryDequeue(ctx)
	require.NoError(t, err)
	assert.NotNil(t, third, "释放后第二单应可取")
}

// TestQueue_GlobalConcurrencyGuard 全局并行上限命中时取不到新单。
func TestQueue_GlobalConcurrencyGuard(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	lm := NewLockManager(client, DefaultLockConfig())
	q := NewQueue(svc, lm, LockConfig{
		MaxConcurrentOrders: 1, MaxConcurrentPerNode: 1,
		LockTimeout: time.Minute, QueuePollInterval: time.Second,
	})

	// 已有一个 running 的变更单（占满全局并行上限）。
	r, err := svc.CreateDraft(ctx, CreateInput{Title: "running", Type: "config", TargetNodes: []int{9}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, r.ID))
	require.NoError(t, svc.Start(ctx, r.ID))

	p, err := svc.CreateDraft(ctx, CreateInput{Title: "pending", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, p.ID))

	got, err := q.TryDequeue(ctx)
	require.NoError(t, err)
	assert.Nil(t, got, "全局并行上限命中，应取不到")
}
