// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package watcher 实现 Agent 侧的 Nginx 配置文件监听器（T029）。
// 用 fsnotify 递归监听配置目录（如 /etc/nginx），在发生写/建/删时经防抖汇总，
// 再通过 Handler 回调通知上层（上层触发配置树重新采集并上报控制面，控制面据此做漂移检测）。
//
// 设计要点：
//   - 防抖：编辑器保存可能产生多个连续事件，全部在窗口内合并为一次回调（同一路径仅一次）。
//   - 噪声过滤：临时文件（.swp/~/.tmp/.bak）、日志轮转产物（.gz/.1）、vim 临时文件（4913）等忽略。
//   - 降级：inotify watch 数量超限（ENOSPC）时自动退化为定时轮询，避免彻底失聪。
//   - 不自动修复：只上报，绝不修改节点配置。
package watcher

import (
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Op 是配置变更操作的分类。
type Op string

const (
	OpWrite  Op = "write"  // 文件内容被修改
	OpCreate Op = "create" // 新建文件或目录
	OpRemove Op = "remove" // 文件或目录被删除
	OpRename Op = "rename" // 文件被重命名
	OpChmod  Op = "chmod"  // 权限/属性变化（通常无需处理）
)

// ConfigChangeEvent 是监听器对外抛出的一个变更事件。
type ConfigChangeEvent struct {
	Path  string    // 发生变化的绝对路径
	Op    Op        // 操作类型
	Time  time.Time // 监听器捕获到该事件的本地时间
	Actor string    // 尽力而为：谁改的（暂留空，auditd 归因属后续增强）
}

// Handler 是变更事件的回调。实现方应快速返回，重活放到自己的 goroutine。
type Handler func(event ConfigChangeEvent)

// Filter 决定某路径是否纳入监听。返回 true 表示保留（产生事件），false 表示丢弃（噪声）。
type Filter func(path string) bool

// Option 用于定制 Watcher 行为。
type Option func(*Watcher)

// WithDebounce 设置防抖窗口（默认 3s）。窗口内对同一路径的多次事件合并为一次回调。
func WithDebounce(d time.Duration) Option {
	return func(w *Watcher) { w.debounce = d }
}

// WithFilters 追加额外的路径过滤器（与内置 DefaultFilter 取交集，全部通过才保留）。
func WithFilters(filters ...Filter) Option {
	return func(w *Watcher) { w.filters = append(w.filters, filters...) }
}

// WithLogger 注入日志器（默认 slog.Default）。
func WithLogger(l *slog.Logger) Option {
	return func(w *Watcher) { w.log = l }
}

// WithPollInterval 设置降级轮询模式下两次全量扫描的间隔（默认 30s）。
func WithPollInterval(d time.Duration) Option {
	return func(w *Watcher) { w.pollInterval = d }
}

// Watcher 递归监听一组路径下的配置文件变化。
type Watcher struct {
	paths        []string
	handler      Handler
	filters      []Filter
	debounce     time.Duration
	pollInterval time.Duration
	log          *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingEntry // 路径 → 待触发事件（防抖窗口内累积）
	timer   *time.Timer
	stopped bool
}

// pendingEntry 记录防抖窗口内某路径累积的变更（取最近一次操作与最新时间）。
type pendingEntry struct {
	op    Op
	last  time.Time
	first time.Time
}

// NewWatcher 构造监听器。paths 为待监听的根目录（递归）。handler 必填。
func NewWatcher(paths []string, handler Handler, opts ...Option) (*Watcher, error) {
	if handler == nil {
		return nil, errNilHandler
	}
	if len(paths) == 0 {
		return nil, errNoPaths
	}
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		clean = append(clean, filepath.Clean(p))
	}
	if len(clean) == 0 {
		return nil, errNoPaths
	}
	w := &Watcher{
		paths:        clean,
		handler:      handler,
		debounce:     3 * time.Second,
		pollInterval: 30 * time.Second,
		log:          slog.Default(),
		pending:      make(map[string]*pendingEntry),
	}
	for _, o := range opts {
		o(w)
	}
	return w, nil
}

// DefaultFilter 丢弃明显的噪声路径：编辑器临时文件、备份、日志轮转产物等。
// 返回 true 表示"值得关注"，false 表示"应忽略"。
func DefaultFilter(path string) bool {
	base := filepath.Base(path)
	lower := strings.ToLower(base)
	// vim / 编辑器临时文件
	if strings.HasSuffix(base, ".swp") || strings.HasSuffix(base, ".swx") ||
		strings.HasSuffix(base, "~") || strings.HasSuffix(base, ".tmp") ||
		strings.HasSuffix(base, ".bak") || strings.HasSuffix(base, ".old") ||
		strings.HasSuffix(base, ".orig") || strings.HasPrefix(base, ".#") {
		return false
	}
	// 日志轮转与压缩产物
	if strings.HasSuffix(lower, ".gz") || strings.HasSuffix(lower, ".log") ||
		strings.HasSuffix(lower, ".1") || strings.HasSuffix(lower, ".2") {
		return false
	}
	// vim 保存瞬间创建的 4913 探测文件
	if base == "4913" {
		return false
	}
	// nginx 运行时临时目录（非配置），避免无谓噪声
	seg := strings.Split(path, string(filepath.Separator))
	for _, s := range seg {
		if s == "client_body_temp" || s == "proxy_temp" || s == "fastcgi_temp" ||
			s == "uwsgi_temp" || s == "scgi_temp" || s == ".git" {
			return false
		}
	}
	return true
}

// accept 组合内置与自定义过滤器，全部通过才保留该路径。
func (w *Watcher) accept(path string) bool {
	if !DefaultFilter(path) {
		return false
	}
	for _, f := range w.filters {
		if !f(path) {
			return false
		}
	}
	return true
}

// ingest 收到一个原始变更，记入防抖队列并（重）启动定时器。
// 同一路径在窗口内的多次事件只会在 flush 时产生一次回调（取最近操作）。
func (w *Watcher) ingest(path string, op Op) {
	if !w.accept(path) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	now := time.Now()
	if e, ok := w.pending[path]; ok {
		e.op = op
		e.last = now
	} else {
		w.pending[path] = &pendingEntry{op: op, first: now, last: now}
	}
	if w.timer == nil {
		w.timer = time.AfterFunc(w.debounce, w.flush)
	} else {
		w.timer.Reset(w.debounce)
	}
}

// flush 在防抖窗口结束后触发：把累积的待处理路径一次性回调，并重置状态。
// 由 time.AfterFunc 在独立 goroutine 调用，不直接持有 w.mu。
func (w *Watcher) flush() {
	w.mu.Lock()
	batch := w.pending
	w.pending = make(map[string]*pendingEntry)
	w.timer = nil
	w.mu.Unlock()

	for path, e := range batch {
		w.handler(ConfigChangeEvent{
			Path: path,
			Op:   e.op,
			Time: e.last,
		})
	}
}

// Stop 立即停止监听并释放底层资源。可重复调用。
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.pending = make(map[string]*pendingEntry)
}
