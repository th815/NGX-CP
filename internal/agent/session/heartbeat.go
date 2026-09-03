package session

import (
	"context"
	"time"
)

// StartScanner 启动心跳超时扫描：每隔 tick 检查所有会话，若距 LastSeen 超过 timeout
// 仍无心跳，则回调 markOffline(nodeID) 将节点标记为离线（online → offline）。
//
// 关键陷阱（见 docs/tasks/M1-skeleton.md T015）：
//   - 超时判定必须用**控制面本地时间**（time.Since(LastSeen)），绝不用 Agent 上报的时间戳，
//     否则时钟歪的节点会永远"在线"。
//   - 扫描器只负责把 DB 状态翻成 offline，**不主动关闭会话**——流的断开由 gRPC 自身的
//     ctx 取消 + handler 的 defer Unregister 处理，避免误杀仍在传输的连接（goroutine 泄漏防护）。
//
// 典型 tick 取 timeout 的 1/3（例如 timeout=30s → tick=10s），保证 30s 内至少扫到一次。
func (m *SessionManager) StartScanner(ctx context.Context, tick, timeout time.Duration, markOffline func(nodeID int)) {
	if tick <= 0 {
		tick = timeout / 3
		if tick <= 0 {
			tick = 10 * time.Second
		}
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			for _, s := range m.snapshot() {
				if now.Sub(s.LastSeen) > timeout {
					if markOffline != nil {
						markOffline(s.NodeID)
					}
				}
			}
		}
	}
}
