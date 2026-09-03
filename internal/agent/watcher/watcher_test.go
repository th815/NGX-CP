// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// collectHandler 累计收到事件的次数与路径集合（线程安全）。
type collectHandler struct {
	mu    sync.Mutex
	count int
	paths map[string]Op
}

func (c *collectHandler) fn(evt ConfigChangeEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	if c.paths == nil {
		c.paths = make(map[string]Op)
	}
	c.paths[evt.Path] = evt.Op
}

func (c *collectHandler) snapshot() (int, map[string]Op) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make(map[string]Op, len(c.paths))
	for k, v := range c.paths {
		cp[k] = v
	}
	return c.count, cp
}

// TestWatcher_CreateModifyRemove 验证创建/修改/删除都能产生对应事件。
func TestWatcher_CreateModifyRemove(t *testing.T) {
	dir := t.TempDir()
	h := &collectHandler{}
	w, err := NewWatcher([]string{dir}, h.fn, WithDebounce(150*time.Millisecond))
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	fp := filepath.Join(dir, "api.conf")
	// fsnotify 建看有启动延迟：若首写发生在 watch 建立前会漏掉 create，
	// 故在轮询窗口内未观察到就重写（已就绪后必产生 write 事件）。
	seen := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mustWrite(t, fp, "server { listen 80; }")
		if waitForEvent(t, h, fp, 300*time.Millisecond) {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatal("未在超时内收到 create/write 事件")
	}
	// 修改
	mustWrite(t, fp, "server { listen 8080; }")
	if !waitForOp(t, h, fp, OpWrite, 2*time.Second) {
		t.Fatal("未在超时内收到 write 事件")
	}
	// 删除
	if err := os.Remove(fp); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !waitForOp(t, h, fp, OpRemove, 2*time.Second) {
		t.Fatal("未在超时内收到 remove 事件")
	}
}

// TestWatcher_FilterNoise 验证 .swp 等临时文件被过滤，不产生事件。
func TestWatcher_FilterNoise(t *testing.T) {
	dir := t.TempDir()
	h := &collectHandler{}
	w, err := NewWatcher([]string{dir}, h.fn, WithDebounce(120*time.Millisecond))
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	// 编辑器临时文件：不应产生事件
	mustWrite(t, filepath.Join(dir, ".api.conf.swp"), "junk")
	mustWrite(t, filepath.Join(dir, "api.conf~"), "junk")
	mustWrite(t, filepath.Join(dir, "4913"), "junk")

	// 真实配置文件：应产生事件
	real := filepath.Join(dir, "api.conf")
	mustWrite(t, real, "server {}")

	time.Sleep(500 * time.Millisecond) // 给防抖与过滤充分时间

	total, paths := h.snapshot()
	if total == 0 {
		t.Fatal("真实配置文件未产生任何事件")
	}
	if _, ok := paths[real]; !ok {
		t.Fatalf("真实文件 api.conf 未上报，已上报: %v", paths)
	}
	for p := range paths {
		if filepath.Base(p) == ".api.conf.swp" || filepath.Base(p) == "api.conf~" || filepath.Base(p) == "4913" {
			t.Fatalf("噪声文件不应被上报: %s", p)
		}
	}
}

// TestWatcher_DebounceCoalesce 验证连续 5 次写只触发 1 次回调（防抖生效）。
func TestWatcher_DebounceCoalesce(t *testing.T) {
	dir := t.TempDir()
	var calls int64
	var mu sync.Mutex
	got := make(map[string]Op)
	w, err := NewWatcher([]string{dir}, func(evt ConfigChangeEvent) {
		atomic.AddInt64(&calls, 1)
		mu.Lock()
		got[evt.Path] = evt.Op
		mu.Unlock()
	}, WithDebounce(300*time.Millisecond))
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	fp := filepath.Join(dir, "api.conf")
	for i := 0; i < 5; i++ {
		mustWrite(t, fp, "write")
		time.Sleep(20 * time.Millisecond) // 远小于防抖窗口
	}

	// 等待防抖窗口结束 + 余量
	time.Sleep(700 * time.Millisecond)
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("期望同路径连续写仅触发 1 次回调，实际 %d", got)
	}
	mu.Lock()
	op := got[fp]
	mu.Unlock()
	if op != OpWrite && op != OpCreate {
		t.Fatalf("期望 write/create 操作，实际 %q", op)
	}
}

// TestWatcher_StopIdempotent 验证 Stop 可安全重复调用且不 panic。
func TestWatcher_StopIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWatcher([]string{dir}, func(ConfigChangeEvent) {}, WithDebounce(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = w.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)
	w.Stop()
	w.Stop() // 重复调用不应 panic
	cancel()
}

// TestWatcher_NewErrors 验证构造参数校验。
func TestWatcher_NewErrors(t *testing.T) {
	if _, err := NewWatcher([]string{"/etc/nginx"}, nil); err != errNilHandler {
		t.Fatalf("期望 errNilHandler，实际 %v", err)
	}
	if _, err := NewWatcher(nil, func(ConfigChangeEvent) {}); err != errNoPaths {
		t.Fatalf("期望 errNoPaths，实际 %v", err)
	}
}

// ---- helpers ----

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// waitForEvent 轮询直到 h 收到任意一次关于 path 的事件。
func waitForEvent(t *testing.T, h *collectHandler, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, paths := h.snapshot()
		if _, ok := paths[path]; ok {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// waitForOp 轮询直到 h 收到关于 path 且操作为 op 的事件。
func waitForOp(t *testing.T, h *collectHandler, path string, op Op, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, paths := h.snapshot()
		if got, ok := paths[path]; ok && got == op {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
