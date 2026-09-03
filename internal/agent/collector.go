// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package agent（采集器子模块）实现 Agent 侧能力/配置采集逻辑：在受管主机上只读执行命令，
// 经 internal/agent/capability 解析为领域结构后，映射为控制面 gRPC 契约（gen/agent/v1）所需的
// proto 报告。所有命令经 hostexec 抽象，单测注入 fake 即可覆盖，无需真实操作系统命令。
//
// 采集始终是只读的——它只"看见"配置，绝不修改系统；私钥等敏感文件内容仅在路径层面记录，
// 不读取、不回传（见 capability 包与 ent NodeConfigFile 设计说明）。
package agent

import (
	"context"
	"log/slog"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent/capability"
	"github.com/th/ngxcp/internal/agent/hostexec"
)

// Collector 封装 Agent 侧能力/配置的采集与 proto 映射。
type Collector struct {
	exec hostexec.CommandExecutor
	log  *slog.Logger
}

// NewCollector 构造采集器。exec 抽象命令执行（真实用 hostexec.NewRealExecutor）。
func NewCollector(exec hostexec.CommandExecutor, log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	return &Collector{exec: exec, log: log}
}

// CollectCapability 采集完整能力基线：nginx -V 画像 + 主机系统信息。
// 非 nginx 节点（纯 LVS Director）nginx 不可执行时，能力基线不含 nginx 部分，
// 但系统信息照常采集——两者解耦，互不阻断。
func (c *Collector) CollectCapability(ctx context.Context) (*agentv1.Capability, error) {
	cap := &agentv1.Capability{}
	if ng, err := capability.CollectNginxV(ctx, c.exec); err != nil {
		c.log.Debug("nginx -V 不可用，按非 nginx 节点处理", "err", err)
	} else {
		cap.Nginx = nginxInfoToProto(ng)
	}
	cap.System = systemInfoToProto(capability.CollectSystemInfo(ctx, c.exec))
	return cap, nil
}

// CollectConfigTree 运行 nginx -T 并采集配置树（含内容，供 T021 内容寻址版本化存储）。
// 注意：nginx -T 仅 dump 配置文件原文（不含 ssl_certificate_key 等私钥文件内容，
// 私钥以路径形式在配置中引用，不进入 -T 输出），故回传配置内容不构成密钥泄露。
func (c *Collector) CollectConfigTree(ctx context.Context) (*agentv1.ConfigTreeReport, error) {
	files, err := capability.CollectNginxTree(ctx, c.exec)
	if err != nil {
		return nil, err
	}
	rep := &agentv1.ConfigTreeReport{CapturedAt: time.Now().Unix()}
	for _, f := range files {
		rep.Files = append(rep.Files, &agentv1.ConfigFile{
			Path:    f.Path,
			Sha256:  f.SHA256,
			Size:    f.Size,
			Content: f.Content, // T021：带上内容，控制面据此做内容寻址与版本链
		})
	}
	return rep, nil
}

// CollectLogTargets 从 nginx -T 配置树提取日志采集目标（含 inode 等运行时 stat）。
// 决定「Agent 内置 tail 该监控哪些文件」：off/syslog 目标跳过，变量路径只标记不展开。
func (c *Collector) CollectLogTargets(ctx context.Context) (*agentv1.LogTargetsReport, error) {
	files, err := capability.CollectNginxTree(ctx, c.exec)
	if err != nil {
		return nil, err
	}
	targets := capability.StatLogTargets(capability.ExtractLogTargets(files))
	rep := &agentv1.LogTargetsReport{CapturedAt: time.Now().Unix()}
	for _, t := range targets {
		rep.Items = append(rep.Items, logTargetToProto(t))
	}
	return rep, nil
}

// ---- proto 映射（领域结构 → gen/agent/v1，单一事实来源在 capability 包）----

func nginxInfoToProto(ng *capability.NginxInfo) *agentv1.NginxInfo {
	if ng == nil {
		return nil
	}
	return &agentv1.NginxInfo{
		Version:        ng.Version,
		BinaryPath:     ng.BinaryPath,
		ConfigureArgs:  ng.ConfigureArgs,
		Prefix:         ng.Prefix,
		ConfPath:       ng.ConfPath,
		SbinPath:       ng.SbinPath,
		PidPath:        ng.PidPath,
		LockPath:       ng.LockPath,
		ErrorLogPath:   ng.ErrorLogPath,
		HttpLogPath:    ng.HTTPLogPath,
		RunUser:        ng.RunUser,
		RunGroup:       ng.RunGroup,
		Compiler:       ng.Compiler,
		OpensslVersion: ng.OpenSSLVersion,
		TlsSni:         ng.TLSSNI,
		StaticModules:  ng.StaticModules,
		DynamicModules: ng.DynamicModules,
		ConfigHash:     ng.ConfigHash,
	}
}

func systemInfoToProto(si capability.SystemInfo) *agentv1.SystemInfo {
	return &agentv1.SystemInfo{
		Os:             si.OS,
		Kernel:         si.Kernel,
		NginxManagedBy: si.NginxManagedBy,
		SelinuxStatus:  si.SELinuxStatus,
		UlimitNofile:   int64(si.UlimitNofile),
		Timezone:       si.Timezone,
		NtpSynced:      si.NTPSynced,
		LogrotateConf:  si.LogRotateConf,
		DiskFree:       si.DiskFree,
		Warnings:       si.Warnings,
	}
}

func logTargetToProto(t capability.LogTarget) *agentv1.LogTarget {
	return &agentv1.LogTarget{
		Path:        t.Path,
		Type:        t.Type,
		Format:      t.Format,
		Level:       t.Level,
		IsSyslog:    t.IsSyslog,
		IsOff:       t.IsOff,
		HasVariable: t.HasVariable,
		SkipReason:  t.SkipReason,
		Size:        t.Size,
		Inode:       t.Inode,
		StatErr:     t.StatErr,
	}
}
