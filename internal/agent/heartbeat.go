// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 tianhao
//
// Package agent 实现 Agent 侧常驻逻辑（与控制面交互的客户端）。
// 当前落地 T015 的心跳客户端与 T018/T019 的健康探测上报：
//   - 维持到控制面的双向心跳长连接，周期上报并响应下发指令；
//   - 断线后按指数退避重连，避免网络恢复瞬间所有 Agent 同时冲垮控制面；
//   - 收到 RUN_COMPLIANCE 指令 → 运行 DR 合规自检并经心跳流上报 ComplianceReport；
//   - 周期性（FsProbeInterval）运行日志/FS 健康探测并经心跳流上报 FsProbeReport；
//   - 周期性运行 nginx -T 采集配置树与日志目标，分别经心跳流上报 ConfigTreeReport / LogTargetsReport；
//   - 收到 REFRESH_CAPABILITY 指令 → 重跑能力发现并经 ReportCapability RPC 上报。
//
// 单一发送者约束：心跳流 stream.Send 只能在主循环 goroutine 调用，避免 SendMsg 并发 panic；
// 探测结果经 channel 汇集到主循环统一发送。
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent/watcher"
)

// HeartbeatConfig 心跳客户端参数（由控制面 Register 响应下发，与 proto ServerConfig 对齐）。
type HeartbeatConfig struct {
	Interval        time.Duration // 上报间隔
	Timeout         time.Duration // 服务端超时（仅用于日志/诊断，客户端不发超时）
	ReconnectBase   time.Duration // 重连退避基数
	ReconnectMax    time.Duration // 重连退避上限
	ClockSkewWarn   time.Duration // 时钟偏差告警阈值（控制面侧计算，客户端保留以对齐契约）
	FsProbeInterval time.Duration // 日志/FS 健康探测上报周期；≤0 时取 6×Interval
}

// HeartbeatCallbacks 是控制面可经心跳驱动 Agent 执行的采集回调集合。
// 各回调独立可选：nil 表示对应采集能力未启用。
type HeartbeatCallbacks struct {
	// ReportCapability 重跑能力发现（nginx -V / -T）并经 ReportCapability RPC 上报。
	ReportCapability func(ctx context.Context) error
	// ReportCompliance 运行 DR 合规自检并返回报告（经心跳流 COMPLIANCE 上报）。
	ReportCompliance func(ctx context.Context) (*agentv1.ComplianceReport, error)
	// ReportFsProbe 运行日志/FS 健康探测并返回报告（经心跳流 FS_PROBE 上报）。
	ReportFsProbe func(ctx context.Context) (*agentv1.FsProbeReport, error)
	// ReportConfigTree 运行 nginx -T 并采集配置树元数据（经心跳流 CONFIG_TREE 上报）。
	ReportConfigTree func(ctx context.Context) (*agentv1.ConfigTreeReport, error)
	// ReportLogTargets 从配置提取日志采集目标（经心跳流 LOG_TARGETS 上报）。
	ReportLogTargets func(ctx context.Context) (*agentv1.LogTargetsReport, error)
	// ValidateConfig 在本地跑 nginx -t 校验（T024），经心跳流 CONFIG_VALIDATE 回传结果。
	ValidateConfig func(ctx context.Context, task *agentv1.ValidateTask) (*agentv1.ValidateResult, error)
}

// Heartbeater 管理一条到控制面的心跳长连接。
type Heartbeater struct {
	cli agentv1.AgentServiceClient
	cfg HeartbeatConfig
	cb  HeartbeatCallbacks
	log *slog.Logger

	// configTreeTrigger 由文件监听器（T029）在检测到配置变更时触发，立即经心跳流上报一次配置树，
	// 控制面据此做漂移检测。非阻塞、可丢弃（缓冲 1，满则丢弃最旧）。
	configTreeTrigger chan struct{}
}

// NewHeartbeater 构造心跳客户端。
func NewHeartbeater(cli agentv1.AgentServiceClient, cfg HeartbeatConfig, cb HeartbeatCallbacks, log *slog.Logger) *Heartbeater {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.ReconnectBase <= 0 {
		cfg.ReconnectBase = time.Second
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = 60 * time.Second
	}
	if cfg.FsProbeInterval <= 0 {
		cfg.FsProbeInterval = 6 * cfg.Interval
	}
	return &Heartbeater{cli: cli, cfg: cfg, cb: cb, log: log, configTreeTrigger: make(chan struct{}, 1)}
}

// TriggerConfigTreePush 由文件监听器（T029）在检测到配置变更后调用，触发一次即时配置树上报。
// 非阻塞：若上一次触发尚未被主循环消费，则丢弃本次（下一次周期/触发会再覆盖）。
func (h *Heartbeater) TriggerConfigTreePush() {
	select {
	case h.configTreeTrigger <- struct{}{}:
	default:
	}
}

// Run 启动心跳循环，直到 ctx 取消。内部对每次连接失败做指数退避重连。
func (h *Heartbeater) Run(ctx context.Context) error {
	backoff := h.cfg.ReconnectBase
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := h.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err() // 主动取消，直接退出
		}
		if err != nil {
			h.log.Warn("heartbeat session ended, reconnecting",
				"err", err, "backoff", backoff)
		}

		// 退避等待；ctx 取消可立即返回，避免退出时挂起。
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		// 指数退避封顶 ReconnectMax（1s → 2s → 4s … → 60s）。
		backoff *= 2
		if backoff > h.cfg.ReconnectMax {
			backoff = h.cfg.ReconnectMax
		}
	}
}

// session 维护单次心跳连接：开流 → 单 goroutine 收指令 → ticker 周期发 PING，
// 并周期运行 FS 健康探测、按需运行合规自检，所有发送统一在主循环进行。
// 返回时连接已结束（错误 or ctx 取消）。
func (h *Heartbeater) session(ctx context.Context) error {
	stream, err := h.cli.Heartbeat(ctx)
	if err != nil {
		return fmt.Errorf("open heartbeat stream: %w", err)
	}

	// 合规自检触发信号（RUN_COMPLIANCE 指令 → 主循环消费并上报）。
	pendingCompliance := make(chan struct{}, 1)
	// FS 健康探测结果汇集通道（探测 goroutine → 主循环发送）。
	fsProbeOut := make(chan *agentv1.FsProbeReport, 1)
	// 配置树 / 日志目标汇集通道（周期采集 goroutine → 主循环发送）。
	configTreeOut := make(chan *agentv1.ConfigTreeReport, 1)
	logTargetsOut := make(chan *agentv1.LogTargetsReport, 1)
	// 校验结果汇集通道（VALIDATE_CONFIG 指令触发 → 主循环经 CONFIG_VALIDATE 上报）。
	validateOut := make(chan *agentv1.ValidateResult, 1)

	// 收指令 goroutine：避免在主循环里 Recv 阻塞发送。
	recvDone := make(chan error, 1)
	go func() {
		for {
			resp, rerr := stream.Recv()
			if rerr != nil {
				recvDone <- rerr
				return
			}
			h.handleCommand(ctx, resp, pendingCompliance, validateOut)
		}
	}()

	// FS 健康探测 goroutine：首跳立即跑一次，之后按 FsProbeInterval 周期运行。
	// 探测在独立 goroutine 串行执行，结果非阻塞推入 fsProbeOut（满则丢弃旧值）。
	go func() {
		runFs := func() {
			if h.cb.ReportFsProbe == nil {
				return
			}
			rep, rerr := h.cb.ReportFsProbe(ctx)
			if rerr != nil {
				h.log.Warn("fs probe failed", "err", rerr)
				return
			}
			select {
			case fsProbeOut <- rep:
			default:
			}
		}
		runFs() // 首跳
		ticker := time.NewTicker(h.cfg.FsProbeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runFs()
			}
		}
	}()

	// 配置树采集 goroutine：首跳立即跑一次，之后按 FsProbeInterval 周期运行。
	go func() {
		run := func() {
			if h.cb.ReportConfigTree == nil {
				return
			}
			rep, rerr := h.cb.ReportConfigTree(ctx)
			if rerr != nil {
				h.log.Warn("config tree collect failed", "err", rerr)
				return
			}
			select {
			case configTreeOut <- rep:
			default:
			}
		}
		run() // 首跳
		ticker := time.NewTicker(h.cfg.FsProbeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()

	// 日志采集目标 goroutine：首跳 + 周期。
	go func() {
		run := func() {
			if h.cb.ReportLogTargets == nil {
				return
			}
			rep, rerr := h.cb.ReportLogTargets(ctx)
			if rerr != nil {
				h.log.Warn("log targets collect failed", "err", rerr)
				return
			}
			select {
			case logTargetsOut <- rep:
			default:
			}
		}
		run() // 首跳
		ticker := time.NewTicker(h.cfg.FsProbeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()

	// 首发一次 PING，让控制面立即感知上线。
	if err := h.sendPing(stream); err != nil {
		return err
	}

	ticker := time.NewTicker(h.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case rerr := <-recvDone:
			return fmt.Errorf("recv: %w", rerr)
		case <-ticker.C:
			if err := h.sendPing(stream); err != nil {
				return err
			}
		case <-pendingCompliance:
			if h.cb.ReportCompliance != nil {
				rep, rerr := h.cb.ReportCompliance(ctx)
				if rerr != nil {
					h.log.Warn("compliance report failed", "err", rerr)
					continue
				}
				if serr := h.sendReport(stream, agentv1.HeartbeatRequest_COMPLIANCE, rep); serr != nil {
					return serr
				}
			}
		case rep := <-fsProbeOut:
			if serr := h.sendReport(stream, agentv1.HeartbeatRequest_FS_PROBE, rep); serr != nil {
				return serr
			}
		case rep := <-configTreeOut:
			if serr := h.sendReport(stream, agentv1.HeartbeatRequest_CONFIG_TREE, rep); serr != nil {
				return serr
			}
		case <-h.configTreeTrigger:
			// T029：文件监听器触发，立即重新采集并上报配置树（控制面据此做漂移检测）。
			if h.cb.ReportConfigTree != nil {
				rep, rerr := h.cb.ReportConfigTree(ctx)
				if rerr != nil {
					h.log.Warn("config tree collect failed (triggered)", "err", rerr)
					continue
				}
				if serr := h.sendReport(stream, agentv1.HeartbeatRequest_CONFIG_TREE, rep); serr != nil {
					return serr
				}
			}
		case rep := <-logTargetsOut:
			if serr := h.sendReport(stream, agentv1.HeartbeatRequest_LOG_TARGETS, rep); serr != nil {
				return serr
			}
		case rep := <-validateOut:
			if serr := h.sendReport(stream, agentv1.HeartbeatRequest_CONFIG_VALIDATE, rep); serr != nil {
				return serr
			}
		}
	}
}

// handleCommand 处理控制面下发的指令。
func (h *Heartbeater) handleCommand(ctx context.Context, resp *agentv1.HeartbeatResponse, pendingCompliance chan struct{}, validateOut chan *agentv1.ValidateResult) {
	switch resp.GetCommand() {
	case agentv1.HeartbeatResponse_REFRESH_CAPABILITY:
		h.log.Info("control-plane requested capability refresh")
		if h.cb.ReportCapability != nil {
			go func() {
				if err := h.cb.ReportCapability(ctx); err != nil {
					h.log.Warn("capability report failed", "err", err)
				}
			}()
		}
	case agentv1.HeartbeatResponse_RUN_COMPLIANCE:
		h.log.Info("control-plane requested compliance self-check")
		// 非阻塞入队：若主循环尚未消费上一次，则丢弃本次（下一次周期/指令会再触发）。
		select {
		case pendingCompliance <- struct{}{}:
		default:
		}
	case agentv1.HeartbeatResponse_VALIDATE_CONFIG:
		h.log.Info("control-plane requested config validation")
		task := resp.GetValidateTask()
		if task == nil || h.cb.ValidateConfig == nil {
			return
		}
		// 在收指令 goroutine 内异步执行（nginx -t 可能耗时），结果经 validateOut 由主循环上报。
		go func() {
			res, err := h.cb.ValidateConfig(ctx, task)
			if err != nil {
				h.log.Warn("config validation failed", "err", err)
				res = &agentv1.ValidateResult{TaskId: task.GetTaskId(), Ok: false, Raw: err.Error()}
			}
			select {
			case validateOut <- res:
			default:
			}
		}()
	default:
		// NONE 等：无需动作。
	}
}

// WatchConfigChanges 启动配置文件监听器（T029）：节点上 nginx 配置被改动时，经心跳流立即触发一次
// 配置树上报，控制面据此做漂移检测。监听在 ctx 取消时自动停止。返回监听器以便上层主动 Stop。
func (h *Heartbeater) WatchConfigChanges(ctx context.Context, paths []string) (*watcher.Watcher, error) {
	w, err := watcher.NewWatcher(paths, func(evt watcher.ConfigChangeEvent) {
		h.log.Info("nginx config changed, triggering immediate config-tree push",
			"path", evt.Path, "op", evt.Op)
		h.TriggerConfigTreePush()
	})
	if err != nil {
		return nil, err
	}
	go func() {
		if werr := w.Start(ctx); werr != nil && ctx.Err() == nil {
			h.log.Warn("config watcher exited with error", "err", werr)
		}
	}()
	return w, nil
}

// sendPing 发送一次带本地时间戳的 PING（控制面据此计算时钟偏差）。
func (h *Heartbeater) sendPing(stream agentv1.AgentService_HeartbeatClient) error {
	return stream.Send(&agentv1.HeartbeatRequest{
		Type:      agentv1.HeartbeatRequest_PING,
		Timestamp: time.Now().Unix(),
	})
}

// sendReport 发送一份健康探测报告（合规自检或日志/FS 健康探测）。
func (h *Heartbeater) sendReport(stream agentv1.AgentService_HeartbeatClient, typ agentv1.HeartbeatRequest_Type, payload any) error {
	req := &agentv1.HeartbeatRequest{
		Type:      typ,
		Timestamp: time.Now().Unix(),
	}
	switch typ {
	case agentv1.HeartbeatRequest_COMPLIANCE:
		req.Compliance = payload.(*agentv1.ComplianceReport)
	case agentv1.HeartbeatRequest_FS_PROBE:
		req.FsProbe = payload.(*agentv1.FsProbeReport)
	case agentv1.HeartbeatRequest_CONFIG_TREE:
		req.ConfigTree = payload.(*agentv1.ConfigTreeReport)
	case agentv1.HeartbeatRequest_LOG_TARGETS:
		req.LogTargets = payload.(*agentv1.LogTargetsReport)
	case agentv1.HeartbeatRequest_CONFIG_VALIDATE:
		req.ValidateResult = payload.(*agentv1.ValidateResult)
	}
	return stream.Send(req)
}
