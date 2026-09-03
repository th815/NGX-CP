package session

import (
	"context"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
)

func newSession(nodeID int) *Session {
	return &Session{
		NodeID:   nodeID,
		LastSeen: time.Now(),
		CmdCh:    make(chan *agentv1.HeartbeatResponse, 4),
	}
}

func TestSessionLifecycle(t *testing.T) {
	m := NewSessionManager(nil)
	a, b := newSession(1), newSession(2)
	m.Register(1, a)
	m.Register(2, b)
	if !m.IsOnline(1) || !m.IsOnline(2) {
		t.Fatal("两节点应均在线")
	}

	// 下发指令：应进入 CmdCh 且被消费者收到（单写 goroutine 语义）。
	cmd := &agentv1.HeartbeatResponse{Command: agentv1.HeartbeatResponse_REFRESH_CAPABILITY}
	if err := m.Send(1, cmd); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case got := <-a.CmdCh:
		if got.Command != agentv1.HeartbeatResponse_REFRESH_CAPABILITY {
			t.Errorf("指令命令 = %v", got.Command)
		}
	case <-time.After(time.Second):
		t.Fatal("指令未送达 CmdCh")
	}

	// 时钟偏差记录与查询。
	m.SetClockSkew(1, 1500*time.Millisecond)
	if sk := m.ClockSkew(1); sk != 1500*time.Millisecond {
		t.Errorf("ClockSkew = %v", sk)
	}
	if sk, ok := m.ClockSkewSeconds(1); !ok || sk != 1.5 {
		t.Errorf("ClockSkewSeconds = %v ok=%v", sk, ok)
	}

	m.Unregister(1)
	if m.IsOnline(1) {
		t.Error("注销后不应在线")
	}
	if !m.IsOnline(2) {
		t.Error("节点 2 应仍在线")
	}
}

// TestScannerMarksOffline 验证：超过 timeout 无心跳的会话会被 markOffline 回调标记，
// 且判定基于控制面本地时间（LastSeen），不依赖任何外部时钟。
func TestScannerMarksOffline(t *testing.T) {
	m := NewSessionManager(nil)
	s := newSession(7)
	// 模拟"很久没心跳"：LastSeen 设为 1 分钟前。
	s.LastSeen = time.Now().Add(-time.Minute)
	m.Register(7, s)

	var mu sync.Mutex
	offlined := map[int]bool{}
	mark := func(id int) {
		mu.Lock()
		offlined[id] = true
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// tick 极短、timeout=100ms → 第一个 tick 必然超时。
	go m.StartScanner(ctx, 10*time.Millisecond, 100*time.Millisecond, mark)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		done := offlined[7]
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("扫描器未在预期内标记 offline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSendToMissingSession 验证：向不存在的会话下发指令返回 ErrSessionNotFound。
func TestSendToMissingSession(t *testing.T) {
	m := NewSessionManager(nil)
	if err := m.Send(99, &agentv1.HeartbeatResponse{}); err != ErrSessionNotFound {
		t.Errorf("期望 ErrSessionNotFound, 实际 %v", err)
	}
}

// TestSendChannelFull 验证：CmdCh 满时 Send 非阻塞返回 ErrSessionBusy（不阻塞热路径）。
func TestSendChannelFull(t *testing.T) {
	m := NewSessionManager(nil)
	s := newSession(3) // CmdCh 缓冲 4
	m.Register(3, s)
	for i := 0; i < 4; i++ {
		if err := m.Send(3, &agentv1.HeartbeatResponse{}); err != nil {
			t.Fatalf("前 4 条应入队: %v", err)
		}
	}
	if err := m.Send(3, &agentv1.HeartbeatResponse{}); err != ErrSessionBusy {
		t.Errorf("第 5 条应返回 ErrSessionBusy, 实际 %v", err)
	}
}
