// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 集成验收（T039）：验证「最小可用闭环」——
// create → submit(审批/免审批) → worker 执行 → Complete 收敛终态，
// 并发路径下节点锁释放无泄漏，回滚可进入 rolling_back。
package deploy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/ent/changeorder"
	"github.com/th/ngxcp/ent/schema"
)

// recordingSink 记录事件，用于验收「实时事件」是否按预期发出（T037 SSE 的数据源）。
type recordingSink struct {
	mu     sync.Mutex
	events []DeployEvent
}

func (s *recordingSink) Emit(e DeployEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *recordingSink) steps() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Step+":"+e.Status)
	}
	return out
}

// newTestWorker 构造测试用 Queue + Worker（runner 注入）。
func newTestWorker(svc *Service, lm *LockManager, runner Runner) (*Worker, *Queue) {
	q := NewQueue(svc, lm, LockConfig{
		MaxConcurrentOrders: 3, MaxConcurrentPerNode: 1,
		LockTimeout: time.Minute, QueuePollInterval: 20 * time.Millisecond,
	})
	return NewWorker(q, svc, runner, q.cfg), q
}

// TestClosedLoop_AutoApprove 免审批闭环：提交 → pending → worker 执行 → success，事件齐全。
func TestClosedLoop_AutoApprove(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	sink := &recordingSink{}
	svc.SetEventSink(sink)
	lm := NewLockManager(client, DefaultLockConfig())
	runner := &fakeOrderRunner{}
	w, _ := newTestWorker(svc, lm, runner)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "cl", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID)) // → pending

	go w.Start(ctx)
	defer w.Stop()

	require.Eventually(t, func() bool {
		got, e := svc.Get(ctx, co.ID)
		return e == nil && got.Status == changeorder.StatusSuccess
	}, 3*time.Second, 20*time.Millisecond, "变更单应闭环收敛到 success")

	steps := sink.steps()
	assert.Contains(t, steps, "submit:"+string(StatusPending))
	assert.Contains(t, steps, "start:"+string(StatusRunning))
	assert.Contains(t, steps, "complete:"+string(StatusSuccess))

	// 节点锁必须释放，无泄漏。
	held, err := lm.HeldBy(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 0, held, "执行完成后节点锁应释放")
}

// TestClosedLoop_WithApproval 需审批闭环：提交 → pending_approval → 批准 → pending → 执行 → success。
func TestClosedLoop_WithApproval(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	svc.SetApprovalConfig(DefaultApprovalConfig())
	lm := NewLockManager(client, DefaultLockConfig())
	runner := &fakeOrderRunner{}
	w, _ := newTestWorker(svc, lm, runner)

	co, err := svc.CreateDraft(ctx, CreateInput{
		Title:       "cl-approve",
		Type:        "config",
		TargetNodes: []int{1},
		CreatedBy:   "admin",
		Strategy:    schema.DeployStrategy{ApprovalRequired: true}, // 强制需审批
	})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID)) // → pending_approval
	got, _ := svc.Get(ctx, co.ID)
	require.Equal(t, string(StatusPendingApproval), string(got.Status))

	go w.Start(ctx)
	defer w.Stop()

	// 自审批拦截：审批人 == 提交人 应被拒（默认 AllowSelfApproval=false）。
	require.Error(t, svc.Approve(ctx, co.ID, "admin"))
	require.NoError(t, svc.Approve(ctx, co.ID, "approver-z")) // → pending

	require.Eventually(t, func() bool {
		g, e := svc.Get(ctx, co.ID)
		return e == nil && g.Status == changeorder.StatusSuccess
	}, 3*time.Second, 20*time.Millisecond, "审批后 worker 应执行并收敛到 success")
}

// TestClosedLoop_Rollback 失败 → 回滚：running/failed → rolling_back。
func TestClosedLoop_Rollback(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	lm := NewLockManager(client, DefaultLockConfig())
	runner := &fakeOrderRunner{err: errors.New("boom")} // 执行器报错 → failed
	w, _ := newTestWorker(svc, lm, runner)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "rb", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))
	go w.Start(ctx)
	defer w.Stop()

	require.Eventually(t, func() bool {
		g, e := svc.Get(ctx, co.ID)
		return e == nil && g.Status == changeorder.StatusFailed
	}, 3*time.Second, 20*time.Millisecond, "执行失败后应收敛到 failed")

	require.NoError(t, svc.StartRollback(ctx, co.ID))
	got, _ := svc.Get(ctx, co.ID)
	assert.Equal(t, string(StatusRollingBack), string(got.Status))
}
