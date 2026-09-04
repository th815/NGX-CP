// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package schema 定义发布引擎的 ent 实体。
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// DeployNodeLock 是节点级互斥锁的持久化行（T038 并发控制）。
//
// 同一 node_id 唯一 → 天然实现「单节点串行」硬约束（一个节点同时只有一个变更单在执行）。
// expires_at 防死锁：控制面崩溃后由 worker / 启动清理回收过期锁。
// 锁的获取在事务内「先清过期 + 再插入」，数据库唯一约束保证跨进程互斥，
// SQLite（事务）与 PG 行为一致，无需 SKIP LOCKED 即可保证正确性。
type DeployNodeLock struct {
	ent.Schema
}

func (DeployNodeLock) Fields() []ent.Field {
	return []ent.Field{
		field.Int("node_id").Unique(),
		field.Int("order_id"),
		field.Time("locked_at").Default(time.Now),
		field.Time("expires_at"),
	}
}

func (DeployNodeLock) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("expires_at"),
		index.Fields("order_id"),
	}
}
