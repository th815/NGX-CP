// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package server 实现发布事件进程内发布/订阅中心（T037 SSE 后端）。
//
// Hub 同时实现 deploy.EventSink（供域层 Service 注入），与
// handler.DeployEventSource（供 SSE 处理器订阅）。设计取舍：
//   - 内存实现 + 有界历史回放，满足 SSE 断连重连（Last-Event-ID）的即时恢复；
//   - 事件「同时落库」用于历史审计与跨进程重放，推到 T039 集成验收一并落地
//     （届时 Hub 接 PG 持久化，对外接口不变）。

package server

import (
	"sync"
	"time"

	"github.com/th/ngxcp/internal/domain/deploy"
)

// Hub 发布事件中心。
type Hub struct {
	mu          sync.RWMutex
	subs        map[int]*hubSub
	nextSubID   int
	history     map[int][]deploy.DeployEvent // orderID -> 最近事件（有界）
	nextEventID int64
	historyCap  int
}

type hubSub struct {
	orderID int
	ch      chan deploy.DeployEvent
}

// NewHub 构造 Hub。historyCap 为每变更单保留的最近事件数（用于重连回放）。
func NewHub(historyCap int) *Hub {
	if historyCap <= 0 {
		historyCap = 256
	}
	return &Hub{
		subs:       make(map[int]*hubSub),
		history:    make(map[int][]deploy.DeployEvent),
		historyCap: historyCap,
	}
}

// Emit 实现 deploy.EventSink：分配事件号、写入历史、广播给该变更单的订阅者。
func (h *Hub) Emit(evt deploy.DeployEvent) {
	h.mu.Lock()
	h.nextEventID++
	evt.ID = h.nextEventID
	if evt.Timestamp == 0 {
		evt.Timestamp = time.Now().UnixMilli()
	}
	// 历史有界裁剪
	hist := append(h.history[evt.OrderID], evt)
	if len(hist) > h.historyCap {
		hist = hist[len(hist)-h.historyCap:]
	}
	h.history[evt.OrderID] = hist

	// 复制当前订阅者，避免持锁广播
	subs := make([]*hubSub, 0, len(h.subs))
	for _, s := range h.subs {
		if s.orderID == evt.OrderID {
			subs = append(subs, s)
		}
	}
	h.mu.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- evt:
		default:
			// 订阅者消费慢：丢弃本次推送，避免阻塞发布路径；
			// SSE 客户端重连后会从历史缓冲补帧。
		}
	}
}

// Subscribe 订阅某变更单的事件流，立即回放当前有界历史后转入实时。
// 返回订阅 id（用于 Unsubscribe）与只读事件通道。
func (h *Hub) Subscribe(orderID int) (int, <-chan deploy.DeployEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextSubID++
	id := h.nextSubID
	hist := h.history[orderID]
	// 缓冲 = 历史长度 + 余量，保证回放不阻塞
	ch := make(chan deploy.DeployEvent, len(hist)+16)
	for _, e := range hist {
		ch <- e
	}
	h.subs[id] = &hubSub{orderID: orderID, ch: ch}
	return id, ch
}

// Unsubscribe 取消订阅并释放资源（SSE 断连时调用，避免内存泄漏）。
func (h *Hub) Unsubscribe(subID int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[subID]; ok {
		delete(h.subs, subID)
	}
}

// subscriberCount 仅供单测断言订阅清理（无锁，测试内自行保证无并发）。
func (h *Hub) subscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}
