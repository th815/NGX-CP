// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package acme 的本地限流（M4 T042）：避免短时间内重复签发触发 Let's Encrypt
// 生产限流（50 张/域名/周）。平台侧统一节流，调试走 staging / pebble 时仍可防自锁。
package acme

import (
	"context"
	"sync"
	"time"
)

// RateLimiter 按 key（通常为域名）保证最小签发间隔的本地限流器。
type RateLimiter struct {
	mu          sync.Mutex
	last        map[string]time.Time
	minInterval time.Duration
}

// NewRateLimiter 构造限流器，minInterval 为同一 key 两次签发的最小间隔。
func NewRateLimiter(minInterval time.Duration) *RateLimiter {
	if minInterval <= 0 {
		minInterval = 6 * time.Second
	}
	return &RateLimiter{last: make(map[string]time.Time), minInterval: minInterval}
}

// Wait 阻塞直到距该 key 上次签发已满 minInterval（受 ctx 取消约束）。
func (r *RateLimiter) Wait(ctx context.Context, key string) error {
	r.mu.Lock()
	if prev, ok := r.last[key]; ok {
		if wait := r.minInterval - time.Since(prev); wait > 0 {
			timer := time.NewTimer(wait)
			r.mu.Unlock()
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			r.mu.Lock()
		}
	}
	r.last[key] = time.Now()
	r.mu.Unlock()
	return nil
}
