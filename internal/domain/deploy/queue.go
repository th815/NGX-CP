// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 实现并发控制（T038）：节点级互斥锁 + 任务队列 + 执行 worker。
package deploy

import (
	"context"

	"github.com/th/ngxcp/ent"
)

// Queue 是发布任务队列：从 pending 变更单中挑出「能抢到全部目标节点锁」的单子交给 worker。
//
// 这是 PG `FOR UPDATE SKIP LOCKED` 的可移植等价实现：在单控制面进程下，逐个尝试 pending 单的节点锁，
// 抢不到全部节点锁的单子跳过看下一个；PG 生产环境的 SKIP LOCKED 是性能优化项（接口与行为不变）。
// 锁本身由数据库唯一约束保证跨进程互斥，因此「正确性」不依赖 SKIP LOCKED。
type Queue struct {
	svc   *Service
	locks *LockManager
	cfg   LockConfig
}

// NewQueue 构造队列。
func NewQueue(svc *Service, locks *LockManager, cfg LockConfig) *Queue {
	return &Queue{svc: svc, locks: locks, cfg: cfg}
}

// TryDequeue 取出一个可立即执行的 pending 变更单，并为其抢下全部目标节点锁。
// 命中全局并行上限或所有 pending 都抢不到锁时返回 (nil, nil)。
func (q *Queue) TryDequeue(ctx context.Context) (*ent.ChangeOrder, error) {
	if q.cfg.MaxConcurrentOrders > 0 {
		running, err := q.svc.ListByStatus(ctx, string(StatusRunning))
		if err != nil {
			return nil, err
		}
		if len(running) >= q.cfg.MaxConcurrentOrders {
			return nil, nil
		}
	}
	pending, err := q.svc.ListByStatus(ctx, string(StatusPending))
	if err != nil {
		return nil, err
	}
	for _, co := range pending {
		ok, err := q.acquireAll(ctx, co)
		if err != nil {
			return nil, err
		}
		if ok {
			return co, nil
		}
		// 抢不到全部节点锁 → 跳过，看下一个 pending 单
	}
	return nil, nil
}

// acquireAll 为变更单抢下全部目标节点锁；任一失败则回滚已抢部分。
func (q *Queue) acquireAll(ctx context.Context, co *ent.ChangeOrder) (bool, error) {
	held := make([]int, 0, len(co.TargetNodes))
	for _, n := range co.TargetNodes {
		ok, err := q.locks.Acquire(ctx, n, co.ID, q.cfg.LockTimeout)
		if err != nil {
			q.releaseHeld(ctx, held, co.ID)
			return false, err
		}
		if !ok {
			q.releaseHeld(ctx, held, co.ID)
			return false, nil
		}
		held = append(held, n)
	}
	return true, nil
}

// releaseHeld 释放已抢到的节点锁（回滚用）。
func (q *Queue) releaseHeld(ctx context.Context, nodes []int, orderID int) {
	for _, n := range nodes {
		_ = q.locks.Release(ctx, n, orderID)
	}
}
