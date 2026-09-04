// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package deploy 实现变更单的持久化与发布编排（T030–T039）。
//
// strategy_lvs.go 把 T035 的 LVS 权重摘除式灰度接入发布流程：组合 T032 的 9 步原子
// 落盘（executor.DeployExecutor）+ T033 复合探活 + T035 权重编排（lvs.GracefulDeploy）。
// 通过部署顺序（先摘一台→变更→加回→观测，再摘下一台）保证整个发布过程对用户零 5xx。
package deploy

import (
	"context"
	"time"

	"github.com/th/ngxcp/internal/agent/executor"
	"github.com/th/ngxcp/internal/agent/probe"
	"github.com/th/ngxcp/internal/domain/lvs"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// LVSStrategy 是「LVS 权重摘除式灰度」发布策略。
type LVSStrategy struct {
	deployExe *executor.DeployExecutor
	graceful  *lvs.GracefulDeploy
}

// NewLVSStrategy 构造 LVS 灰度策略。
//   - deployExe：T032 原子落盘执行器（单节点变更）
//   - setter：LVS 权重执行器（Agent 侧 ipvs.IPVSExecutor 满足 lvs.WeightSetter）
//   - probeCfgs：双层探活配置（本地 + 远程 VIP），为空则跳过探活
func NewLVSStrategy(deployExe *executor.DeployExecutor, setter lvs.WeightSetter, probeCfgs ...probe.ProbeConfig) *LVSStrategy {
	probers := make([]probe.Prober, 0, len(probeCfgs))
	for _, c := range probeCfgs {
		if p, err := probe.New(c); err == nil {
			probers = append(probers, p)
		}
	}
	return &LVSStrategy{
		deployExe: deployExe,
		graceful:  &lvs.GracefulDeploy{Setter: setter, Probers: probers},
	}
}

// SetTimings 覆盖灰度时序参数。默认排空 120s / 观测 60s（契约值），单测或特殊场景可缩短。
func (s *LVSStrategy) SetTimings(drain, observe, poll time.Duration) {
	s.graceful.DrainTimeout = drain
	s.graceful.ObserveWindow = observe
	s.graceful.DrainPoll = poll
}

// DeployNodeCanary 对单个节点执行 LVS 灰度发布。
// backend 为该 Nginx 节点在 LVS 中的 RS 地址引用；deployReq 是下发到该节点的 9 步变更请求。
func (s *LVSStrategy) DeployNodeCanary(ctx context.Context, nodeID int, backend lvs.BackendRef, deployReq executor.DeployRequest) error {
	s.graceful.Deployer = func(c context.Context, id int) error {
		_, err := s.deployExe.Deploy(c, deployReq, nil)
		return err
	}
	return s.graceful.DeployOne(ctx, nodeID, backend)
}

// DeployAll 按给定顺序对多个 backend 依次灰度（先摘一台做完再做下一台）。
func (s *LVSStrategy) DeployAll(ctx context.Context, nodes []int, backends []lvs.BackendRef, deployReq executor.DeployRequest) error {
	if len(nodes) != len(backends) {
		return apperr.New(apperr.CodeInternal, "nodes 与 backends 长度不一致")
	}
	for i, b := range backends {
		if err := s.DeployNodeCanary(ctx, nodes[i], b, deployReq); err != nil {
			return err
		}
	}
	return nil
}
