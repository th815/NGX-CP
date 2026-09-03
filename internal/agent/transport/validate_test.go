// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package transport

import (
	"context"
	"log/slog"
	"testing"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// newTestServer 构造仅含校验协调所需的极简 Server（ca/enroll/nodeSvc 在本测试中无需注入）。
func newTestServer(sm *session.SessionManager) *Server {
	return &Server{
		log:           slog.Default(),
		sessions:      sm,
		validateChans: make(map[string]chan *agentv1.ValidateResult),
	}
}

// TestValidateConfig_RoundTrip 验证：下发 VALIDATE_CONFIG 命令 → 经 deliverValidateResult 回传结果。
func TestValidateConfig_RoundTrip(t *testing.T) {
	sm := session.NewSessionManager(slog.Default())
	cmdCh := make(chan *agentv1.HeartbeatResponse, 1) // 测试直接持有，用于读取下发的命令
	sm.Register(1, &session.Session{NodeID: 1, CmdCh: cmdCh})
	srv := newTestServer(sm)

	resCh := make(chan *agentv1.ValidateResult, 1)
	go func() {
		res, err := srv.ValidateConfig(context.Background(), 1, &agentv1.ValidateTask{ConfPath: "nginx.conf"})
		if err != nil {
			t.Errorf("ValidateConfig 返回错误: %v", err)
			return
		}
		resCh <- res
	}()

	// 取出下发命令，校验其形态（命令类型 + task_id 已生成）。
	var cmd *agentv1.HeartbeatResponse
	select {
	case c := <-cmdCh:
		cmd = c
	case <-time.After(2 * time.Second):
		t.Fatal("未下发校验命令")
	}
	if cmd.GetCommand() != agentv1.HeartbeatResponse_VALIDATE_CONFIG {
		t.Fatalf("命令类型错误: %v", cmd.GetCommand())
	}
	taskID := cmd.GetTaskId()
	if taskID == "" {
		t.Fatal("task_id 不应为空")
	}
	if cmd.GetValidateTask().GetConfPath() != "nginx.conf" {
		t.Fatalf("校验任务内容未透传: %+v", cmd.GetValidateTask())
	}

	// Agent 回传结果（模拟心跳流 CONFIG_VALIDATE）。
	srv.deliverValidateResult(taskID, &agentv1.ValidateResult{TaskId: taskID, Ok: true, Raw: "syntax is ok"})

	select {
	case res := <-resCh:
		if !res.GetOk() {
			t.Fatal("期望回传 Ok=true")
		}
		if res.GetTaskId() != taskID {
			t.Fatalf("回传 task_id 不匹配: %s vs %s", res.GetTaskId(), taskID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("结果未回传")
	}
}

// TestValidateConfig_Offline Agent 未在线 → 返回 4103（CodeUnavailable）。
func TestValidateConfig_Offline(t *testing.T) {
	sm := session.NewSessionManager(slog.Default()) // 不为 node 99 注册会话
	srv := newTestServer(sm)
	_, err := srv.ValidateConfig(context.Background(), 99, &agentv1.ValidateTask{ConfPath: "nginx.conf"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if apperr.CodeOf(err) != apperr.CodeUnavailable {
		t.Fatalf("期望 4103，实际 code=%v", apperr.CodeOf(err))
	}
}

// TestValidateConfig_NilTask 任务为空 → 参数非法 4001。
func TestValidateConfig_NilTask(t *testing.T) {
	srv := newTestServer(session.NewSessionManager(slog.Default()))
	_, err := srv.ValidateConfig(context.Background(), 1, nil)
	if apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("期望 4001，实际 code=%v", apperr.CodeOf(err))
	}
}
