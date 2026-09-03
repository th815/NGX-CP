// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 tianhao
//
// Package health 实现 NGX-CP Agent 侧主机健康探测执行器。
//
// 在受管节点（Nginx RS / Keepalived Director）本机执行只读命令
// （ip / arp / sysctl / df / openssl / stat 等），解析输出并组装控制面约定的：
//   - ComplianceReport：DR 合规自检（对应 internal/domain/compliance 规则目录）
//   - FsProbeReport：日志/文件系统健康（对应 internal/domain/probe 规则目录）
// 经心跳流上报后，由控制面聚合两维度判定节点 degraded / online。
//
// 设计约束（软件著作权审查与可测试性）：
//   - 所有外部命令经 CommandExecutor 抽象，默认 RealExecutor 用 os/exec，单测用 fake 注入；
//   - 探测仅读取本机状态，绝不修改系统配置（只读）；
//   - 规则定义（name/severity/期望/修复命令）复用 internal/domain/{compliance,probe} 的 Catalog，
//     保证 Agent / 控制面两侧完全一致，避免重复定义漂移；所有解析逻辑均为项目自研。
package health

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
	// Output 执行命令并返回合并的 stdout+stderr 输出（与 nginx -V 输出到 stderr 的命令一致处理）。
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

// RealExecutor 使用 os/exec 与 os 包在真实主机执行命令与文件操作。
type RealExecutor struct{}

// Output 执行外部命令并合并 stdout/stderr 返回。
func (RealExecutor) Output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Stat 使用 os.Stat 获取文件元信息。
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
		WorldWritable: mode.Perm()&0002 != 0,
	}, nil
}

// Exists 判断路径是否存在。
func (RealExecutor) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsWritableDir 通过创建并删除临时文件验证目录可写（探测日志目录可写性是健康探针的必要最小写操作，立即清理）。
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

// NewRealExecutor 返回默认的真实执行器。
func NewRealExecutor() CommandExecutor { return RealExecutor{} }
