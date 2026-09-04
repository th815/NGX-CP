// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package executor 实现 Agent 在受管主机上执行的能力型命令。
//
// ipvs.go 是 T035 在 LVS Director 节点侧的权重执行器：内部复用 domain/lvs 的真实
// 权重 setter 驱动 ipvsadm。proto 生成后，Heartbeat 的 SET_RS_WEIGHT 命令直接调用
// SetRealServerWeight；本文件即该命令的处理实现（请求结构对齐 proto 的
// SetRealServerWeightTask，生成前先用本地结构，避免阻塞）。
package executor

import (
	"context"
	"fmt"

	"github.com/th/ngxcp/internal/agent/hostexec"
	"github.com/th/ngxcp/internal/domain/lvs"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// IPVSExecutor 是 Agent 在 LVS Director 上操作 RS 权重的执行器。
type IPVSExecutor struct {
	setter lvs.WeightSetter
}

// NewIPVSExecutor 用真实命令执行器构造（Agent 运行于 LVS 节点时使用）。
func NewIPVSExecutor(run hostexec.CommandExecutor) *IPVSExecutor {
	return &IPVSExecutor{setter: &lvs.RealWeightSetter{Exec: run}}
}

// SetRealServerWeightRequest 对应 proto 的 SetRealServerWeightTask（生成前先用本地结构）。
type SetRealServerWeightRequest struct {
	VIP     string // 虚拟 IP，如 192.168.5.5
	VIPPort int    // 虚拟服务端口，如 80 / 443
	Proto   string // TCP / UDP
	RSAddr  string // 真实服务器地址
	RSPort  int    // 真实服务器端口
	Weight  int    // 目标权重（0 = 摘除）
}

// SetRealServerWeight 按请求把指定 RS 在某 VS 上的权重置为 Weight（0=摘除）。
func (e *IPVSExecutor) SetRealServerWeight(ctx context.Context, req SetRealServerWeightRequest) error {
	vs := lvs.VirtualServerRef{Proto: req.Proto, Address: req.VIP, Port: req.VIPPort}
	rs := lvs.RealServerRef{Address: req.RSAddr, Port: req.RSPort}
	if err := e.setter.SetWeight(ctx, vs, rs, req.Weight); err != nil {
		return apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("SET_RS_WEIGHT %s -> %s w=%d 失败", vs, rs, req.Weight), err)
	}
	return nil
}
