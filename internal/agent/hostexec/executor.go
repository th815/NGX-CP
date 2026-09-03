// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package hostexec 抽象 Agent 在受管主机上执行命令与检查文件状态的能力。
//
// 抽成独立包的原因：能力发现（capability）与健康探测（health）都需要跑本机命令，
// 但二者分层不同——capability 是更底层的能力采集，health 依赖它。若把执行器定义在
// health 里会造成 capability 反向依赖 health 的分层倒置，故上提为中立包。
//
// 设计约束（软件著作权审查与可测试性）：
//   - 所有外部命令经 CommandExecutor 抽象：真实运行用 RealExecutor（os/exec），
//     单测用 fake 注入预置输出，无需 root、无需真实操作系统命令；
//   - 探测类调用一律只读，绝不修改系统配置。唯一的例外是 IsWritableDir
//     （写临时文件后立即删除），这是判断目录可写性的必要最小写操作。
package hostexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// CommandExecutor 抽象在主机上执行命令与检查文件状态的能力。
// 单测用 fake 注入预置输出，避免依赖真实操作系统命令。
type CommandExecutor interface {
	// Output 执行命令并返回合并的 stdout+stderr 输出
	// （与 `nginx -V` 这类把结果写到 stderr 的命令保持一致处理）。
	Output(ctx context.Context, name string, args ...string) (string, error)
	// Stat 返回文件/目录精简元信息，用于权限与存在性检查。
	Stat(path string) (FileInfo, error)
	// Exists 判断路径是否存在（文件或目录）。
	Exists(path string) bool
	// IsWritableDir 判断目录是否可写（在其内创建并删除临时文件验证）。
	IsWritableDir(path string) bool
	// ReadDir 返回目录下的文件名列表（不含路径前缀）。
	ReadDir(dir string) ([]string, error)
	// ReadFile 读取文件全部内容（用于 keepalived.conf 等配置解析）。
	ReadFile(path string) (string, error)
}

// FileInfo 是 Stat 返回的精简文件元信息（unix 语义）。
type FileInfo struct {
	Exists        bool
	IsDir         bool
	Mode          os.FileMode
	WorldWritable bool // 全局可写（other write bit 置位）
}

// DeviceID 返回路径所在设备的 ID（用于判定两个目录是否同一文件系统）。
// 平台相关实现见 device_unix.go / device_other.go；取不到时返回 0。
func DeviceID(path string) uint64 { return deviceID(path) }

// RealExecutor 使用 os/exec 与 os 包在真实主机执行命令与文件操作。
type RealExecutor struct{}

// NewRealExecutor 返回默认的真实执行器。
func NewRealExecutor() CommandExecutor { return RealExecutor{} }

// Output 执行外部命令并合并 stdout/stderr 返回。
func (RealExecutor) Output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Stat 使用 os.Stat 获取文件元信息。路径不存在不视为错误（Exists=false）。
func (RealExecutor) Stat(path string) (FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileInfo{Exists: false}, nil
		}
		return FileInfo{}, err
	}
	mode := fi.Mode()
	return FileInfo{
		Exists:        true,
		IsDir:         fi.IsDir(),
		Mode:          mode,
		WorldWritable: mode.Perm()&0o002 != 0,
	}, nil
}

// Exists 判断路径是否存在。
func (RealExecutor) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsWritableDir 通过创建并删除临时文件验证目录可写。
func (RealExecutor) IsWritableDir(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		return false
	}
	tmp := filepath.Join(path, fmt.Sprintf(".ngxcp-probe-%d", os.Getpid()))
	if err := os.WriteFile(tmp, []byte("ok"), 0o600); err != nil {
		return false
	}
	_ = os.Remove(tmp)
	return true
}

// ReadDir 使用 os.ReadDir 返回目录文件名列表。
func (RealExecutor) ReadDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// ReadFile 使用 os.ReadFile 读取文件内容。
func (RealExecutor) ReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
