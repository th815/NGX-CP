// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package lvs 的远程权重执行器（M5 T054）：把 domain/lvs.WeightSetter 接口接到
// 已就绪的 control-plane → Agent 心跳命令通道（transport.Server.SetRSWeight）。
//
// RS 权重操作发生在 LVS Director 节点（ipvsadm 在 Director 上跑），故下发目标是
// 持有该 VS 的 Director 节点，而非 RS 节点本身。ResolveDirectorNode 由控制面装配时
// 基于 ent 模型提供（VirtualService → Director → Node）。
package lvs

import (
	"context"
	"fmt"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/virtualservice"
	domainlvs "github.com/th/ngxcp/internal/domain/lvs"
	"github.com/th/ngxcp/internal/agent/transport"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// RemoteSetter 经 Agent 心跳命令通道驱动 Director 节点上的 ipvsadm，实现 domain/lvs.WeightSetter。
// 它让控制面侧的 GracefulDeploy 编排（internal/domain/lvs/graceful.go）能在分布式环境驱动权重。
type RemoteSetter struct {
	Transport           *transport.Server
	ResolveDirectorNode DirectorResolver
}

// DirectorResolver 把 VS 解析到其所属 Director 的受管节点 ID。
type DirectorResolver func(ctx context.Context, vs domainlvs.VirtualServerRef) (nodeID int, err error)

// NewRemoteSetter 构造远程权重执行器。
func NewRemoteSetter(t *transport.Server, resolve DirectorResolver) *RemoteSetter {
	return &RemoteSetter{Transport: t, ResolveDirectorNode: resolve}
}

// SetWeight 把 rs 在某 VS 上的权重经 Agent 下发到对应 Director 节点（w=0 即摘除）。
func (r *RemoteSetter) SetWeight(ctx context.Context, vs domainlvs.VirtualServerRef, rs domainlvs.RealServerRef, weight int) error {
	if r.Transport == nil {
		return apperr.New(apperr.CodeUnavailable, "控制面未接入 Agent 下发通道（transport.Server 缺失）")
	}
	if r.ResolveDirectorNode == nil {
		return apperr.New(apperr.CodeInternal, "未配置 Director 节点解析器")
	}
	nodeID, err := r.ResolveDirectorNode(ctx, vs)
	if err != nil {
		return apperr.Wrap(apperr.CodePrecondition, fmt.Sprintf("解析 VS %s 所属 Director 失败", vs), err)
	}
	task := &agentv1.SetRealServerWeightTask{
		Vip:     vs.Address,
		VipPort: int32(vs.Port),
		Proto:   vs.Proto,
		RsAddr:  rs.Address,
		RsPort:  int32(rs.Port),
		Weight:  int32(weight),
	}
	res, err := r.Transport.SetRSWeight(ctx, nodeID, task)
	if err != nil {
		return err
	}
	if res == nil || !res.GetOk() {
		msg := "Agent 返回失败（无原因）"
		if res != nil && res.GetError() != "" {
			msg = res.GetError()
		}
		return apperr.New(apperr.CodeUnavailable, fmt.Sprintf("Director 节点 %d 调权失败：%s", nodeID, msg))
	}
	return nil
}

// ListVirtualServers 返回模型期望的 VS/RS 视图（非运行时查询）。
// 注意：当前控制面掌握期望状态（desired state），运行时 ipvs 查询通道（LIST_VS）尚未建立；
// 完整 7 步灰度的排空检测（ActiveConn）依赖运行时，待 LIST_VS 接入后启用。
func (r *RemoteSetter) ListVirtualServers(ctx context.Context) ([]domainlvs.VirtualServer, error) {
	return nil, apperr.New(apperr.CodeUnavailable, "运行时 VS 查询未接入（LIST_VS 命令待建）；当前仅支持按模型驱动的权重编排")
}

// ResolveDirectorNode 基于 ent 模型把 VS 解析到 Director 节点 ID。
func ResolveDirectorNode(client *ent.Client) DirectorResolver {
	return func(ctx context.Context, vs domainlvs.VirtualServerRef) (int, error) {
		vsEnt, err := client.VirtualService.Query().
			Where(
				virtualservice.Vip(vs.Address),
				virtualservice.Port(vs.Port),
			).
			WithDirector(func(q *ent.DirectorQuery) {
				q.WithNode()
			}).
			Only(ctx)
		if err != nil {
			if ent.IsNotFound(err) {
				return 0, apperr.New(apperr.CodeNotFound, fmt.Sprintf("找不到 VS %s", vs))
			}
			return 0, err
		}
		if vsEnt.Edges.Director == nil {
			return 0, apperr.New(apperr.CodePrecondition, fmt.Sprintf("VS %s 未关联 Director", vs))
		}
		d := vsEnt.Edges.Director
		if d.Edges.Node == nil {
			return 0, apperr.New(apperr.CodePrecondition, fmt.Sprintf("Director %d 未关联节点", d.ID))
		}
		return d.Edges.Node.ID, nil
	}
}
