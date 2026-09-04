// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package server 的发布执行闭环接线（T039）：把批准的变更单经 Agent gRPC 心跳流推到目标节点，
// 由 Agent 跑 9 步原子落盘，并据回传的 DeployProgress 收敛订单终态（success / failed）。
// 此前 Worker 注入 runner=nil，订单进 running 后停在「等待执行器接入」；本文件补齐真实执行器。
package server

import (
	"context"
	"fmt"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	configstore "github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/domain/deploy"
)

// DeployDispatcher 是控制面经心跳命令流下发部署指令的窄接口（由 *transport.Server 实现）。
// 用窄接口隔离域层与传输层，便于单测注入 fake。
type DeployDispatcher interface {
	DeployConfig(ctx context.Context, nodeID int, task *agentv1.SyncConfigTask) (*agentv1.DeployProgress, error)
}

// deployFileSource 是读取节点托管配置文件的窄接口（由 *configstore.ConfigStore 实现）。
type deployFileSource interface {
	ListFiles(ctx context.Context, nodeID int) ([]*configstore.FileView, error)
	GetCurrentContent(ctx context.Context, fileID int) ([]byte, error)
}

// AgentRunner 实现 deploy.Runner：逐节点构建 SyncConfigTask 并下发，任一节点失败即整单失败。
type AgentRunner struct {
	svc      *deploy.Service
	files    deployFileSource
	dispatch DeployDispatcher
}

// NewAgentRunner 构造 Agent 执行器。
func NewAgentRunner(svc *deploy.Service, files deployFileSource, d DeployDispatcher) *AgentRunner {
	return &AgentRunner{svc: svc, files: files, dispatch: d}
}

// Run 执行单条变更单：对每个目标节点拉取托管配置并下发 9 步原子落盘，依回传结果收敛。
func (r *AgentRunner) Run(ctx context.Context, orderID int) error {
	co, err := r.svc.Get(ctx, orderID)
	if err != nil {
		return err
	}
	for _, nodeID := range co.TargetNodes {
		task, berr := r.buildTask(ctx, orderID, nodeID)
		if berr != nil {
			return fmt.Errorf("构建节点 %d 部署任务失败: %w", nodeID, berr)
		}
		res, derr := r.dispatch.DeployConfig(ctx, nodeID, task)
		if derr != nil {
			return fmt.Errorf("节点 %d 部署指令下发失败: %w", nodeID, derr)
		}
		if res == nil || res.GetStatus() == "failed" {
			return fmt.Errorf("节点 %d 部署失败: %s", nodeID, res.GetMessage())
		}
	}
	return nil
}

// buildTask 拉取目标节点的全部托管配置，组装成 Agent 可执行的 SyncConfigTask。
// prefix/nginx_path/conf_path 留空，由 Agent 用本地配置默认值（与控制面同源、无需重复下发）。
func (r *AgentRunner) buildTask(ctx context.Context, orderID, nodeID int) (*agentv1.SyncConfigTask, error) {
	files, err := r.files.ListFiles(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]*agentv1.FileToWrite, 0, len(files))
	for _, f := range files {
		if f.Path == "" {
			continue
		}
		content, cerr := r.files.GetCurrentContent(ctx, f.ID)
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, &agentv1.FileToWrite{
			Path:    f.Path,
			Content: string(content),
			Sha256:  f.CurrentSHA,
		})
	}
	return &agentv1.SyncConfigTask{
		ChangeOrderId:     int64(orderID),
		NodeId:            int64(nodeID),
		Files:             out,
		ObserveWindowSec:  5,
	}, nil
}
