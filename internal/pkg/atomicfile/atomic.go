// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package atomicfile 提供「要么完全生效、要么完全不变」的文件写入原语。
//
// 发布引擎的原子切换（T032 步骤⑤）依赖它：同文件系统用 rename（原子），
// 跨文件系统降级为 copy+unlink，从而保证切换中途失败时可从快照恢复、
// 不会出现「部分文件已生效」的中间态。
package atomicfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// SameDevice 判断两个路径是否位于同一文件系统（通过 stat 的 Dev 字段）。
// 任一路径取不到时返回 false（保守地走跨盘降级路径）。
func SameDevice(a, b string) bool {
	da, ok1 := deviceOf(a)
	db, ok2 := deviceOf(b)
	return ok1 && ok2 && da == db
}

func deviceOf(path string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

// MoveFile 把 src 移动到 dst：
//   - 同文件系统：os.Rename（原子）；
//   - 跨文件系统：copy 内容到 dst 后删除 src（降级，非原子但保证最终一致）。
//
// dst 的父目录会被创建。调用方需自行保证 src 已通过校验。
func MoveFile(src, dst string) error {
	// 比设备时取 dst 的父目录（dst 通常尚不存在，rename 落地取决于其父目录所在文件系统）。
	if SameDevice(src, filepath.Dir(dst)) {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("创建目标目录 %s 失败: %w", filepath.Dir(dst), err)
		}
		if err := os.Rename(src, dst); err == nil {
			return nil
		}
		// rename 失败（极少数：dst 跨 mount 边界）落到 copy 降级。
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	// 降级路径下删除源；rename 路径下源已被移走，Remove 报错可忽略。
	_ = os.Remove(src)
	return nil
}

// WriteFile 把 data 原子写入 dst（同盘 temp+rename；跨盘降级 copy）。
// 写入前会创建父目录。返回是否走了同盘原子路径（true=rename，false=copy 降级）。
func WriteFile(dst string, data []byte) (bool, error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}
	same := SameDevice(dir, filepath.Dir(dst))
	if same {
		tmp, err := os.CreateTemp(dir, ".ngxcp-atomic-*")
		if err != nil {
			return false, fmt.Errorf("创建临时文件失败: %w", err)
		}
		tmpName := tmp.Name()
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			_ = os.Remove(tmpName)
			return false, fmt.Errorf("写临时文件失败: %w", err)
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpName)
			return false, fmt.Errorf("关闭临时文件失败: %w", err)
		}
		if err := os.Chmod(tmpName, 0o600); err != nil {
			_ = os.Remove(tmpName)
			return false, fmt.Errorf("设临时文件权限失败: %w", err)
		}
		if err := os.Rename(tmpName, dst); err != nil {
			_ = os.Remove(tmpName)
			// 落到跨盘降级
		} else {
			return true, nil
		}
	}
	if err := copyFileBytes(dst, data); err != nil {
		return false, err
	}
	return false, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件 %s 失败: %w", src, err)
	}
	defer in.Close()
	data, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("读取源文件 %s 失败: %w", src, err)
	}
	return copyFileBytes(dst, data)
}

func copyFileBytes(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("创建目标目录 %s 失败: %w", filepath.Dir(dst), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".ngxcp-atomic-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("写临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("设临时文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("替换目标文件 %s 失败: %w", dst, err)
	}
	return nil
}
