package logtail

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// loadOffset 读取持久化的读取偏移量。文件不存在视为从 0 开始。
// 文件内容损坏（非整数）视为从 0 开始（宁可重读，不可跳过）。
func loadOffset(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// 损坏的 offset 文件：回退到 0，避免跳过日志。
		return 0, nil
	}
	if n < 0 {
		return 0, nil
	}
	return n, nil
}

// saveOffset 原子写入偏移量：先写临时文件再 rename，
// 保证崩溃时 offset 文件要么旧值要么新值，不会半截。
func saveOffset(path string, off int64) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	data := []byte(strconv.FormatInt(off, 10))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
