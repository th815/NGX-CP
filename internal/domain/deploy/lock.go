// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 实现并发控制（T038）：节点级互斥锁 + 任务队列 + 执行 worker。
package deploy

import (
	"context"
	"time"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/deploynodelock"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// LockConfig 并发控制策略（与任务契约 deploy 段一致）。
type LockConfig struct {
	MaxConcurrentOrders  int           // 全局并行变更单上限；<=0 表示不限制
	MaxConcurrentPerNode int           // 单节点并行上限，硬约束为 1
	LockTimeout          time.Duration // 节点锁过期时间，防死锁（默认 30m）
	QueuePollInterval    time.Duration // worker 轮询间隔（默认 2s）
}

// DefaultLockConfig 内置默认并发策略。
func DefaultLockConfig() LockConfig {
	return LockConfig{
		MaxConcurrentOrders:  3,
		MaxConcurrentPerNode: 1,
		LockTimeout:          30 * time.Minute,
		QueuePollInterval:    2 * time.Second,
	}
}

// LockManager 负责节点锁的获取/释放/过期清理。
// 节点锁为 deploy_node_lock 表的持久化行：node_id 唯一 → 天然实现「单节点串行」。
// 锁在事务内「先清过期 + 再插入」，数据库唯一约束保证跨进程互斥；SQLite（事务）与 PG 行为一致。
type LockManager struct {
	client *ent.Client
	cfg    LockConfig
}

// NewLockManager 构造节点锁管理器。
func NewLockManager(client *ent.Client, cfg LockConfig) *LockManager {
	return &LockManager{client: client, cfg: cfg}
}

// Acquire 尝试为 orderID 抢占 nodeID 的互斥锁，ttl 为锁超时。
// 返回 (true, nil) 表示抢占成功；(false, nil) 表示节点已被占用或锁过期未清理；错误为系统异常。
func (m *LockManager) Acquire(ctx context.Context, nodeID, orderID int, ttl time.Duration) (bool, error) {
	now := time.Now()
	tx, err := m.client.Tx(ctx)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "开启节点锁事务失败", err)
	}
	defer tx.Rollback()
	// 先清理该节点上的过期锁（可能来自崩溃的控制面或历史订单）。
	if _, err := tx.DeployNodeLock.Delete().
		Where(deploynodelock.NodeID(nodeID), deploynodelock.ExpiresAtLT(now)).Exec(ctx); err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "清理过期节点锁失败", err)
	}
	// 唯一约束（node_id）保证「先到先得」，冲突即被占用。
	_, err = tx.DeployNodeLock.Create().
		SetNodeID(nodeID).SetOrderID(orderID).SetExpiresAt(now.Add(ttl)).Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return false, nil
		}
		return false, apperr.Wrap(apperr.CodeInternal, "获取节点锁失败", err)
	}
	if err := tx.Commit(); err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "提交节点锁失败", err)
	}
	return true, nil
}

// Release 释放指定节点上由 orderID 持有的锁（仅持有者可释放）。
func (m *LockManager) Release(ctx context.Context, nodeID, orderID int) error {
	_, err := m.client.DeployNodeLock.Delete().
		Where(deploynodelock.NodeID(nodeID), deploynodelock.OrderID(orderID)).Exec(ctx)
	return err
}

// ReleaseByOrder 释放某变更单持有的全部节点锁（执行完成/取消时调用）。
func (m *LockManager) ReleaseByOrder(ctx context.Context, orderID int) error {
	_, err := m.client.DeployNodeLock.Delete().
		Where(deploynodelock.OrderID(orderID)).Exec(ctx)
	return err
}

// Refresh 延长指定节点锁的过期时间（心跳续租）。
func (m *LockManager) Refresh(ctx context.Context, nodeID, orderID int, ttl time.Duration) error {
	_, err := m.client.DeployNodeLock.Update().
		Where(deploynodelock.NodeID(nodeID), deploynodelock.OrderID(orderID)).
		SetExpiresAt(time.Now().Add(ttl)).Save(ctx)
	return err
}

// HeldBy 返回当前持有 nodeID 锁的 orderID；无有效锁（含已过期）返回 0。
func (m *LockManager) HeldBy(ctx context.Context, nodeID int) (int, error) {
	l, err := m.client.DeployNodeLock.Query().
		Where(deploynodelock.NodeID(nodeID), deploynodelock.ExpiresAtGT(time.Now())).First(ctx)
	if ent.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "查询节点锁失败", err)
	}
	return l.OrderID, nil
}

// CleanExpired 清理全部已过期锁（控制面启动/定时 worker 调用）。返回清理条数。
func (m *LockManager) CleanExpired(ctx context.Context) (int, error) {
	n, err := m.client.DeployNodeLock.Delete().
		Where(deploynodelock.ExpiresAtLT(time.Now())).Exec(ctx)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "清理过期节点锁失败", err)
	}
	return n, nil
}
