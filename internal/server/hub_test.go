// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package server

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/th/ngxcp/internal/domain/deploy"
)

// TestHub_PublishSubscribe 订阅后发布的事件能被实时收到。
func TestHub_PublishSubscribe(t *testing.T) {
	h := NewHub(16)
	id, ch := h.Subscribe(1)
	defer h.Unsubscribe(id)

	h.Emit(deploy.DeployEvent{OrderID: 1, Step: "submit", Status: "pending"})
	select {
	case evt := <-ch:
		assert.Equal(t, 1, evt.OrderID)
		assert.Equal(t, int64(1), evt.ID)
		assert.NotZero(t, evt.Timestamp)
	case <-time.After(time.Second):
		t.Fatal("未在超时内收到事件")
	}
}

// TestHub_NoCrossOrder 只收到本变更单的事件，不串流。
func TestHub_NoCrossOrder(t *testing.T) {
	h := NewHub(16)
	id, ch := h.Subscribe(1)
	defer h.Unsubscribe(id)

	h.Emit(deploy.DeployEvent{OrderID: 2, Step: "x"})
	select {
	case <-ch:
		t.Fatal("不应收到其它变更单的事件")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHub_ReplayHistory 订阅时回放有界历史，迟到客户端也能补齐。
func TestHub_ReplayHistory(t *testing.T) {
	h := NewHub(16)
	h.Emit(deploy.DeployEvent{OrderID: 1, Step: "a"})
	h.Emit(deploy.DeployEvent{OrderID: 1, Step: "b"})

	id, ch := h.Subscribe(1)
	defer h.Unsubscribe(id)

	got := 0
	for i := 0; i < 2; i++ {
		select {
		case e := <-ch:
			assert.Equal(t, 1, e.OrderID)
			got++
		case <-time.After(time.Second):
			t.Fatal("回放超时")
		}
	}
	assert.Equal(t, 2, got)
}

// TestHub_Unsubscribe 取消订阅后资源释放，无泄漏。
func TestHub_Unsubscribe(t *testing.T) {
	h := NewHub(16)
	id, _ := h.Subscribe(1)
	assert.Equal(t, 1, h.subscriberCount())
	h.Unsubscribe(id)
	assert.Equal(t, 0, h.subscriberCount())
}

// TestHub_Race 并发订阅/发布/取消在 -race 下无数据竞争。
func TestHub_Race(t *testing.T) {
	h := NewHub(16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id, ch := h.Subscribe(n % 3)
			defer h.Unsubscribe(id)
			for j := 0; j < 20; j++ {
				h.Emit(deploy.DeployEvent{OrderID: n % 3, Step: "x"})
				select {
				case <-ch:
				default:
				}
			}
		}(i)
	}
	wg.Wait()
}
