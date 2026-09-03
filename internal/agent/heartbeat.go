// Package agent 实现 Agent 侧常驻逻辑（与控制面交互的客户端）。
// 当前落地 T015 的心跳客户端：维持与控制面的双向心跳长连接，周期上报并响应下发指令，
// 断线后按指数退避重连，避免网络恢复瞬间所有 Agent 同时冲垮控制面。
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
)

// HeartbeatConfig 心跳客户端参数（由控制面 Register 响应下发，与 proto ServerConfig 对齐）。
// 字段与 internal/agent/session.HeartbeatConfig 对称，但仅客户端关心的子集生效。
type HeartbeatConfig struct {
	Interval      time.Duration // 上报间隔
	Timeout       time.Duration // 服务端超时（仅用于日志/诊断，客户端不发超时）
	ReconnectBase time.Duration // 重连退避基数
	ReconnectMax  time.Duration // 重连退避上限
	ClockSkewWarn time.Duration // 时钟偏差告警阈值（控制面侧计算，客户端保留以对齐契约）
}

// Heartbeater 管理一条到控制面的心跳长连接。
type Heartbeater struct {
	cli       agentv1.AgentServiceClient
	cfg       HeartbeatConfig
	reportCap func(ctx context.Context) error // 收到 REFRESH_CAPABILITY 时触发能力上报
	log       *slog.Logger
}

// NewHeartbeater 构造心跳客户端。
// reportCap 为可选回调：控制面要求刷新能力时被调用（典型实现是重跑 nginx -V / -T 并 ReportCapability）。
func NewHeartbeater(cli agentv1.AgentServiceClient, cfg HeartbeatConfig, reportCap func(ctx context.Context) error, log *slog.Logger) *Heartbeater {
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
	return &Heartbeater{cli: cli, cfg: cfg, reportCap: reportCap, log: log}
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

// session 维护单次心跳连接：开流 → 单 goroutine 收指令 → ticker 周期发 PING。
// 返回时连接已结束（错误 or ctx 取消）。
func (h *Heartbeater) session(ctx context.Context) error {
	stream, err := h.cli.Heartbeat(ctx)
	if err != nil {
		return fmt.Errorf("open heartbeat stream: %w", err)
	}

	// 收指令 goroutine：避免在主循环里 Recv 阻塞发送。
	recvDone := make(chan error, 1)
	go func() {
		for {
			resp, rerr := stream.Recv()
			if rerr != nil {
				recvDone <- rerr
				return
			}
			h.handleCommand(ctx, resp)
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
		}
	}
}

// handleCommand 处理控制面下发的指令。
func (h *Heartbeater) handleCommand(ctx context.Context, resp *agentv1.HeartbeatResponse) {
	switch resp.GetCommand() {
	case agentv1.HeartbeatResponse_REFRESH_CAPABILITY:
		h.log.Info("control-plane requested capability refresh")
		if h.reportCap != nil {
			if err := h.reportCap(ctx); err != nil {
				h.log.Warn("capability report failed", "err", err)
			}
		}
	default:
		// NONE 等：无需动作。
	}
}

// sendPing 发送一次带本地时间戳的 PING（控制面据此计算时钟偏差）。
func (h *Heartbeater) sendPing(stream agentv1.AgentService_HeartbeatClient) error {
	return stream.Send(&agentv1.HeartbeatRequest{
		Type:      agentv1.HeartbeatRequest_PING,
		Timestamp: time.Now().Unix(),
	})
}
