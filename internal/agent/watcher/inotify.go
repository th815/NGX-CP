// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// fsnotify 后端实现（T029）：递归监听配置目录，并在 inotify watch 数量超限（ENOSPC）
// 时降级为定时轮询，保证监听器在受限环境下仍不"失聪"。
package watcher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

var (
	errNilHandler = errors.New("watcher: handler is nil")
	errNoPaths    = errors.New("watcher: no paths to watch")
)

// Start 启动监听，直到 ctx 取消。
// 优先使用 fsnotify（Linux inotify / macOS kqueue）；若 watch 数超限（ENOSPC）则降级为定时轮询。
func (w *Watcher) Start(ctx context.Context) error {
	fsErr := w.runFsnotify(ctx)
	if fsErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err() // 主动取消，不再降级
	}
	if isENOSPC(fsErr) {
		w.log.Warn("inotify watch limit reached, falling back to polling", "err", fsErr)
		return w.runPolling(ctx)
	}
	return fsErr
}

// runFsnotify 用 fsnotify 递归监听，阻塞直到 ctx 取消或致命错误。
func (w *Watcher) runFsnotify(ctx context.Context) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fw.Close()

	for _, root := range w.paths {
		if aerr := w.addRecursive(ctx, fw, root); aerr != nil {
			return aerr // ENOSPC 会冒泡触发降级
		}
	}

	for {
		select {
		case <-ctx.Done():
			w.Stop()
			return ctx.Err()
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			w.handleFsEvent(ctx, fw, ev)
		case e, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.log.Warn("fsnotify watch error", "err", e)
		}
	}
}

// addRecursive 把 root 下所有目录加入监听；遇到 ENOSPC 立即返回以触发降级。
func (w *Watcher) addRecursive(ctx context.Context, fw *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				return nil // 无权限目录跳过，不致命
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !d.IsDir() {
			return nil
		}
		if aerr := fw.Add(path); aerr != nil {
			if isENOSPC(aerr) {
				return aerr
			}
			w.log.Warn("add watch failed, skipping", "path", path, "err", aerr)
		}
		return nil
	})
}

// handleFsEvent 把 fsnotify 事件映射为内部 Op 并送入防抖队列。
// 新建目录需补加监听（实现"递归"——fsnotify 仅监听已 Add 的目录层）。
func (w *Watcher) handleFsEvent(ctx context.Context, fw *fsnotify.Watcher, ev fsnotify.Event) {
	path := ev.Name
	switch {
	case ev.Op&fsnotify.Create != 0:
		if fi, serr := os.Stat(path); serr == nil && fi.IsDir() {
			// 异步补加新子目录的监听，避免阻塞事件循环。
			go func() {
				_ = w.addRecursive(context.WithoutCancel(ctx), fw, path)
			}()
		}
		w.ingest(path, OpCreate)
	case ev.Op&fsnotify.Remove != 0:
		w.ingest(path, OpRemove)
	case ev.Op&fsnotify.Rename != 0:
		w.ingest(path, OpRename)
	case ev.Op&fsnotify.Write != 0:
		w.ingest(path, OpWrite)
	case ev.Op&fsnotify.Chmod != 0:
		w.ingest(path, OpChmod)
	}
}

// ---- 降级轮询模式 ----

// fileSig 是文件的轻量指纹（mtime + size），用于轮询时判断内容是否变化。
type fileSig struct {
	mod  time.Time
	size int64
}

// runPolling 定时全量扫描目录树，比对指纹后产出变更事件；阻塞直到 ctx 取消。
func (w *Watcher) runPolling(ctx context.Context) error {
	sig := make(map[string]fileSig)
	w.scanTree(sig)
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.Stop()
			return ctx.Err()
		case <-ticker.C:
			w.scanTree(sig)
		}
	}
}

// scanTree 扫描一遍目录树：新增/变更文件发 OpWrite，消失文件发 OpRemove。
func (w *Watcher) scanTree(sig map[string]fileSig) {
	seen := make(map[string]struct{}, len(sig))
	for _, root := range w.paths {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !w.accept(path) {
				return nil
			}
			fi, serr := d.Info()
			if serr != nil {
				return nil
			}
			s := fileSig{mod: fi.ModTime(), size: fi.Size()}
			seen[path] = struct{}{}
			if old, ok := sig[path]; !ok || old != s {
				w.ingest(path, OpWrite)
				sig[path] = s
			}
			return nil
		})
	}
	// 检测被删除的文件：基线里有、本次未见 → 删除事件
	for p := range sig {
		if _, ok := seen[p]; !ok {
			w.ingest(p, OpRemove)
			delete(sig, p)
		}
	}
}

// isENOSPC 判断错误是否由 inotify watch 数量超限（ENOSPC）引起。
func isENOSPC(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	// fsnotify 在不同平台可能以字符串形式暴露该错误。
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "enospc") || strings.Contains(msg, "too many open files")
}
