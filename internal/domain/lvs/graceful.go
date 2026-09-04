// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package lvs 实现 LVS 权重摘除式灰度（T035）。
//
// GracefulDeploy 实现契约的 7 步序列：
//
//	① 快照原权重  ② 摘除(backend 在所有 VS 上 w=0)  ③ 排空(ActiveConn≈0)
//	④ 变更(T032 9 步)  ⑤ 双层探活  ⑥ 加回原权重  ⑦ 观测窗口
//
// 关键不变量：通过 defer 保证「无论成功失败，被摘除的 backend 权重最终都会被加回原值」，
// 绝不让节点因异常永久停留在池外（避免灰度演变成事故）。摘除期流量完全不命中该 backend，
// 因此全程对用户零 5xx。
//
// 注意：同一台物理 RS 在 LVS 中常以多个条目存在（如 :80 / :443(tcp) / :443(udp) 端口不同），
// 故高层按 BackendRef(地址) 枚举其全部 VS 条目统一操作，而非单个 (VS,RS) 配对。
package lvs

import (
	"context"
	"fmt"
	"time"

	"github.com/th/ngxcp/internal/agent/probe"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// NodeDeployer 在单节点执行变更（T032 9 步原子落盘）。返回 error 表示变更失败。
type NodeDeployer func(ctx context.Context, nodeID int) error

// BackendRef 标识一台物理真实服务器（按 IP 地址）。一个 backend 在 LVS 中往往以多个
// RS 条目存在（如 :80 / :443(tcp) / :443(udp)），灰度需对其全部条目统一置权重。
type BackendRef struct {
	Address string
}

// rsEntry 是 (VS, RS) 配对及原权重，灰度期间对该配对整体操作。
type rsEntry struct {
	VS     VirtualServerRef
	RS     RealServerRef
	Origin int
}

// GracefulDeploy 实现 LVS 权重摘除式灰度编排。
type GracefulDeploy struct {
	Setter        WeightSetter
	Deployer      NodeDeployer
	Probers       []probe.Prober // 双层探活：本地 + 远程 VIP，任一不过即失败
	DrainTimeout  time.Duration  // ③ 排空超时，默认 120s
	ObserveWindow time.Duration  // ⑦ 观测窗口，默认 60s
	DrainPoll     time.Duration  // ③ 排空轮询间隔，默认 2s
}

func (d *GracefulDeploy) defaults() {
	if d.DrainTimeout <= 0 {
		d.DrainTimeout = 120 * time.Second
	}
	if d.ObserveWindow <= 0 {
		d.ObserveWindow = 60 * time.Second
	}
	if d.DrainPoll <= 0 {
		d.DrainPoll = 2 * time.Second
	}
}

// DeployOne 对单台 backend 执行完整灰度序列。nodeID 用于驱动底层 9 步变更。
func (d *GracefulDeploy) DeployOne(ctx context.Context, nodeID int, backend BackendRef) error {
	d.defaults()
	if d.Setter == nil || d.Deployer == nil {
		return apperr.New(apperr.CodeInternal, "GracefulDeploy 未注入 Setter/Deployer")
	}

	// ① 枚举 backend 在全部 VS 中的条目并快照原权重
	vss, err := d.Setter.ListVirtualServers(ctx)
	if err != nil {
		return err
	}
	entries := backendEntries(vss, backend)
	if len(entries) == 0 {
		return apperr.Wrap(apperr.CodePrecondition,
			fmt.Sprintf("backend %s 不在任何 VS 中，无法灰度", backend.Address), nil)
	}
	// 安全网：任何返回路径都先把权重加回原值（正常路径在 ⑥ 已加回，此处幂等兜底）
	defer func() { _ = d.restoreEntries(context.Background(), entries) }()

	// ② 摘除：所有条目权重置 0（生产有 80/443tcp/443udp，必须全置）
	if err := d.setEntries(ctx, entries, 0); err != nil {
		return err
	}

	// ③ 排空：轮询 ActiveConn 直到 ≈0 或超时
	if err := d.waitDrained(ctx, entries); err != nil {
		return err
	}

	// ④ 变更：执行 T032 9 步原子落盘（失败则 defer 已加回权重）
	if err := d.Deployer(ctx, nodeID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("节点 %d 变更失败", nodeID), err)
	}

	// ⑤ 双层探活：本地 + 远程 VIP，任一不过即失败（defer 已加回权重）
	if err := d.probe(ctx); err != nil {
		return err
	}

	// ⑥ 加回原权重（探活通过后才加回；与 defer 幂等）
	if err := d.restoreEntries(ctx, entries); err != nil {
		return err
	}

	// ⑦ 观测窗口：期间可对比错误率/延迟/QPS 与基线
	select {
	case <-time.After(d.ObserveWindow):
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// DeployAll 按给定顺序对多个 backend 依次灰度（契约 ⑦：对下一台重复 ①–⑥）。
func (d *GracefulDeploy) DeployAll(ctx context.Context, nodeIDs []int, backends []BackendRef) error {
	if len(nodeIDs) != len(backends) {
		return apperr.New(apperr.CodeInternal, "nodeIDs 与 backends 长度不一致")
	}
	for i, b := range backends {
		if err := d.DeployOne(ctx, nodeIDs[i], b); err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("backend %s 灰度失败", b.Address), err)
		}
	}
	return nil
}

// backendEntries 取出属于该 backend 地址的全部 (VS, RS) 条目及原权重。
func backendEntries(vss []VirtualServer, b BackendRef) []rsEntry {
	var out []rsEntry
	for _, vs := range vss {
		for _, r := range vs.RealServers {
			if r.Ref.Address == b.Address {
				out = append(out, rsEntry{VS: vs.Ref, RS: r.Ref, Origin: r.Weight})
			}
		}
	}
	return out
}

func (d *GracefulDeploy) setEntries(ctx context.Context, entries []rsEntry, w int) error {
	for _, e := range entries {
		if err := d.Setter.SetWeight(ctx, e.VS, e.RS, w); err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("置权重失败 %s -> %s w=%d", e.VS, e.RS, w), err)
		}
	}
	return nil
}

func (d *GracefulDeploy) restoreEntries(ctx context.Context, entries []rsEntry) error {
	var firstErr error
	for _, e := range entries {
		if err := d.Setter.SetWeight(ctx, e.VS, e.RS, e.Origin); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return apperr.Wrap(apperr.CodeInternal,
			"恢复权重失败（节点可能仍被摘除，需人工介入！）", firstErr)
	}
	return nil
}

func (d *GracefulDeploy) waitDrained(ctx context.Context, entries []rsEntry) error {
	deadline := time.Now().Add(d.DrainTimeout)
	for {
		vss, err := d.Setter.ListVirtualServers(ctx)
		if err != nil {
			return err
		}
		if activeOfEntries(vss, entries) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return apperr.Wrap(apperr.CodePrecondition,
				fmt.Sprintf("排空超时（%s 内 ActiveConn 未归零）", d.DrainTimeout), nil)
		}
		select {
		case <-time.After(d.DrainPoll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (d *GracefulDeploy) probe(ctx context.Context) error {
	for _, p := range d.Probers {
		res, err := p.Probe(ctx)
		if err != nil {
			return apperr.Wrap(apperr.CodePrecondition, "灰度探活执行异常", err)
		}
		if res == nil || !res.OK {
			return apperr.Wrap(apperr.CodePrecondition, "灰度探活未通过（节点已变更但探活失败）", nil)
		}
	}
	return nil
}

// activeOfEntries 统计给定条目在所有 VS 上的 ActiveConn 之和（排空判定用）。
func activeOfEntries(vss []VirtualServer, entries []rsEntry) int {
	n := 0
	for _, e := range entries {
		for _, vs := range vss {
			for _, r := range vs.RealServers {
				if r.Ref == e.RS {
					n += r.ActiveConn
				}
			}
		}
	}
	return n
}
