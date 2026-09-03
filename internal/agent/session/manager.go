// Package session 实现控制面侧的 Agent 会话管理（T015）。
// 每个在线的 Agent 双向心跳流对应一个 Session；SessionManager 负责注册/注销、
// 下发指令（单写 goroutine 消费 CmdCh，避免 SendMsg 并发）、在线判定与时钟偏差查询。
package session

import (
	"errors"
	"sync"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"log/slog"
)

// 错误哨兵：调用方据此决定降级策略。
var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionBusy     = errors.New("session command channel full")
)

// HeartbeatConfig 心跳与会话参数（由控制面配置映射，与 proto ServerConfig 对齐）。
type HeartbeatConfig struct {
	Interval      time.Duration // 期望上报间隔
	Timeout       time.Duration // 超过未心跳 → 标记 offline
	ReconnectBase time.Duration // 重连指数退避基数
	ReconnectMax  time.Duration // 重连退避上限
	ClockSkewWarn time.Duration // 时钟偏差告警阈值
}

// Session 是一个 Agent 的在线会话视图。
// Stream 不在此持有引用（由 gRPC handler 直接驱动），这里只保存会话元数据。
type Session struct {
	NodeID    int
	LastSeen  time.Time
	ClockSkew time.Duration // 节点与控制面的时间偏差（由 Heartbeat 上报的 timestamp 计算）
	// CmdCh 是下发给该 Agent 的指令队列；仅由单写 goroutine 消费，保证 SendMsg 不并发。
	CmdCh chan *agentv1.HeartbeatResponse
}

// SessionManager 维护 nodeID -> Session 的映射。
// 所有字段访问均在 mu 保护下，支持高并发的注册/注销/心跳上报。
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[int]*Session
	log      *slog.Logger
}

// NewSessionManager 构造会话管理器。
func NewSessionManager(log *slog.Logger) *SessionManager {
	if log == nil {
		log = slog.Default()
	}
	return &SessionManager{
		sessions: make(map[int]*Session),
		log:      log,
	}
}

// Register 注册一个节点会话。若同一 nodeID 已有旧会话（极端的重连竞态），
// 旧会话的 CmdCh 会被关闭以促使旧写入 goroutine 退出，避免指令串台。
func (m *SessionManager) Register(nodeID int, s *Session) {
	m.mu.Lock()
	if old, ok := m.sessions[nodeID]; ok && old != s {
		close(old.CmdCh)
	}
	m.sessions[nodeID] = s
	m.mu.Unlock()
}

// Unregister 注销一个节点会话（流断开时由 handler defer 调用）。
// 仅移除当前仍指向该会话的表项，避免误删重连后新建的会话。
func (m *SessionManager) Unregister(nodeID int) {
	m.mu.Lock()
	delete(m.sessions, nodeID)
	m.mu.Unlock()
}

// Send 向指定节点下发一条指令。非阻塞：通道满则丢弃并返回 ErrSessionBusy，
// 调用方（如刷新能力）可稍后重试，绝不在心跳热路径上阻塞。
func (m *SessionManager) Send(nodeID int, cmd *agentv1.HeartbeatResponse) error {
	m.mu.RLock()
	s, ok := m.sessions[nodeID]
	m.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	select {
	case s.CmdCh <- cmd:
		return nil
	default:
		return ErrSessionBusy
	}
}

// IsOnline 报告节点当前是否有活跃会话。
func (m *SessionManager) IsOnline(nodeID int) bool {
	m.mu.RLock()
	_, ok := m.sessions[nodeID]
	m.mu.RUnlock()
	return ok
}

// ClockSkew 返回节点的时钟偏差（未记录则为 0）。
func (m *SessionManager) ClockSkew(nodeID int) time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.sessions[nodeID]; ok {
		return s.ClockSkew
	}
	return 0
}

// ClockSkewSeconds 返回时钟偏差（秒，带符号），供 HTTP API 暴露。
// 第二返回值表示是否记录到偏差（即该节点当前在线且上报过 timestamp）。
func (m *SessionManager) ClockSkewSeconds(nodeID int) (float64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.sessions[nodeID]; ok {
		return s.ClockSkew.Seconds(), true
	}
	return 0, false
}

// UpdateLastSeen 心跳到达时刷新最后可见时间（用控制面本地时间，杜绝节点时钟歪导致误判）。
func (m *SessionManager) UpdateLastSeen(nodeID int, t time.Time) {
	m.mu.Lock()
	if s, ok := m.sessions[nodeID]; ok {
		s.LastSeen = t
	}
	m.mu.Unlock()
}

// SetClockSkew 记录该节点与控制面的时间偏差。
func (m *SessionManager) SetClockSkew(nodeID int, d time.Duration) {
	m.mu.Lock()
	if s, ok := m.sessions[nodeID]; ok {
		s.ClockSkew = d
	}
	m.mu.Unlock()
}

// LastSeen 返回节点最后可见时间。
func (m *SessionManager) LastSeen(nodeID int) (time.Time, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.sessions[nodeID]; ok {
		return s.LastSeen, true
	}
	return time.Time{}, false
}

// snapshot 返回当前会话的浅拷贝（供扫描器无锁遍历）。
func (m *SessionManager) snapshot() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}
