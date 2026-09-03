// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoveFile_SameDevice(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	require.NoError(t, os.WriteFile(src, []byte("hello"), 0o600))
	dst := filepath.Join(dir, "sub", "dst.txt") // 目标父目录不存在，应被创建
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))

	require.True(t, SameDevice(src, filepath.Dir(dst)), "同 TempDir 下应同文件系统")
	require.NoError(t, MoveFile(src, dst))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
	_, statErr := os.Stat(src)
	assert.True(t, os.IsNotExist(statErr), "源文件应已被移走")
}

func TestWriteFile_Atomic(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.txt")
	same, err := WriteFile(dst, []byte("payload"))
	require.NoError(t, err)
	assert.True(t, same, "同盘应走 rename 原子路径")

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(got))
}

func TestMoveFile_CrossDeviceFallback(t *testing.T) {
	// 用两个不同 base 目录模拟跨设备：显式让 dst 父目录建在一个独立 TempDir，
	// 但 SameDevice 可能仍为 true（同一文件系统），此测试主要验证 MoveFile 在
	// 任意情况下都能最终一致地把内容送到 dst 并清理 src。
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	src := filepath.Join(dir1, "a.txt")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o600))
	dst := filepath.Join(dir2, "b.txt")

	// 即便跨盘，也应成功并清理源
	require.NoError(t, MoveFile(src, dst))
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
	_, statErr := os.Stat(src)
	assert.True(t, os.IsNotExist(statErr))
}
