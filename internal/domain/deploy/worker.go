// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 实现并发控制（T038）：节点级互斥锁 + 任务队列 + 执行 worker。
package deploy

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/th/ngxcp/ent"
)

// Runner 是单条变更单的实际执行器（T039 经 Agent 接线 9 步流水线后实现）。
type Runner interface {
	Run(ctx context.Context, orderID int) error
}

// Worker 轮询队列并执行变更单：抢占锁 → Start → Runner.Run → 释放锁。
// 自身只负责调度与锁生命周期，执行细节交给注入的 Runner，保证「单节点串行」硬约束。
type Worker struct {
	q      *Queue
	svc    *Service
	runner Runner
	cfg    LockConfig

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewWorker 构造 worker；runner 为 nil 时不会真正执行（仅占位，由 T039 注入）。
func NewWorker(q *Queue, svc *Service, runner Runner, cfg LockConfig) *Worker {
	return &Worker{q: q, svc: svc, runner: runner, cfg: cfg, stopCh: make(chan struct{})}
}

// Start 启动调度循环（阻塞，通常放 goroutine）。ctx 取消即退出。
func (w *Worker) Start(ctx context.Context) {
	w.wg.Add(1)
	defer w.wg.Done()
	for {
		co, err := w.q.TryDequeue(ctx)
		if err != nil {
			log.Error().Err(err).Msg("deploy queue dequeue error")
			if !w.wait(ctx) {
				return
			}
			continue
		}
		if co != nil {
			w.process(ctx, co)
			continue
		}
		if !w.wait(ctx) {
			return
		}
	}
}

// wait 按轮询间隔等待，返回 false 表示应退出（ctx 取消或显式 Stop）。
func (w *Worker) wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-w.stopCh:
		return false
	case <-time.After(w.cfg.QueuePollInterval):
		return true
	}
}

// process 执行单条变更单：Start → Runner → 收敛终态 → 释放节点锁。
func (w *Worker) process(ctx context.Context, co *ent.ChangeOrder) {
	if err := w.svc.Start(ctx, co.ID); err != nil {
		// 已被并发 worker 启动或状态变化 → 释放锁，不重复执行。
		_ = w.q.locks.ReleaseByOrder(ctx, co.ID)
		return
	}
	if w.runner != nil {
		if err := w.runner.Run(ctx, co.ID); err != nil {
			_ = w.svc.Complete(ctx, co.ID, false, err.Error())
		} else {
			_ = w.svc.Complete(ctx, co.ID, true, "执行完成")
		}
	} else {
		// 未注入执行器（生产态 Agent 尚未接入）：仅标记运行，不收敛终态，
		// 避免「假装成功」。真实执行随 Agent 部署落地。
		w.svc.emit(DeployEvent{OrderID: co.ID, Step: "worker", Status: string(StatusRunning), Message: "等待执行器接入"})
	}
	// 无论 Runner 成败，释放节点锁，避免锁泄漏。
	_ = w.q.locks.ReleaseByOrder(ctx, co.ID)
}

// Stop 通知调度循环退出并等待。
func (w *Worker) Stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.wg.Wait()
}
