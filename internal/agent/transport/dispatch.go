// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package transport 的控制面命令下发（T031–T035）：
// 控制面经 Agent 心跳命令通道下发执行型任务（部署/回滚/快照/调权），
// 阻塞等待 Agent 经同一心跳流回传的结果。结果按 task_id 投递，
// 与 T024 的 ValidateConfig 机制同源（见 grpc_server.go）。
package transport

import (
	"context"
	"errors"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// 命令结果等待超时：部署含 9 步原子落盘可能较久，单独放宽；快照/调权走快速路径。
const (
	deployTimeout   = 120 * time.Second
	snapshotTimeout = 30 * time.Second
	rsWeightTimeout = 30 * time.Second
	certTimeout     = 60 * time.Second
)

// DeployConfig 经心跳命令流请求目标 Agent 执行 9 步原子落盘（T032），阻塞等待终态 DeployProgress。
func (s *Server) DeployConfig(ctx context.Context, nodeID int, task *agentv1.SyncConfigTask) (*agentv1.DeployProgress, error) {
	taskID := ensureTaskID(task.GetTaskId())
	task.TaskId = taskID
	cmd := &agentv1.HeartbeatResponse{
		Command:    agentv1.HeartbeatResponse_DEPLOY_CONFIG,
		TaskId:     taskID,
		SyncConfig: task,
	}
	return s.waitDeployResult(ctx, nodeID, cmd, deployTimeout)
}

// RollbackConfig 经心跳命令流请求 Agent 跑回滚流水线（T034），阻塞等待终态 DeployProgress。
func (s *Server) RollbackConfig(ctx context.Context, nodeID int, task *agentv1.RollbackTask) (*agentv1.DeployProgress, error) {
	taskID := ensureTaskID(task.GetTaskId())
	task.TaskId = taskID
	cmd := &agentv1.HeartbeatResponse{
		Command:     agentv1.HeartbeatResponse_ROLLBACK_CONFIG,
		TaskId:      taskID,
		RollbackTask: task,
	}
	return s.waitDeployResult(ctx, nodeID, cmd, deployTimeout)
}

// CreateSnapshot 经心跳命令流请求 Agent 在本地抓配置快照（T031），阻塞等待 SnapshotResult。
func (s *Server) CreateSnapshot(ctx context.Context, nodeID int, task *agentv1.SnapshotCreateTask) (*agentv1.SnapshotResult, error) {
	taskID := ensureTaskID(task.GetTaskId())
	task.TaskId = taskID
	cmd := &agentv1.HeartbeatResponse{
		Command:       agentv1.HeartbeatResponse_CREATE_SNAPSHOT,
		TaskId:        taskID,
		SnapshotCreate: task,
	}
	return s.waitSnapshotResult(ctx, nodeID, cmd, snapshotTimeout)
}

// RestoreSnapshot 经心跳命令流请求 Agent 从快照恢复配置（T031），阻塞等待 SnapshotResult。
func (s *Server) RestoreSnapshot(ctx context.Context, nodeID int, task *agentv1.SnapshotRestoreTask) (*agentv1.SnapshotResult, error) {
	taskID := ensureTaskID(task.GetTaskId())
	task.TaskId = taskID
	cmd := &agentv1.HeartbeatResponse{
		Command:        agentv1.HeartbeatResponse_RESTORE_SNAPSHOT,
		TaskId:         taskID,
		SnapshotRestore: task,
	}
	return s.waitSnapshotResult(ctx, nodeID, cmd, snapshotTimeout)
}

// SetRSWeight 经心跳命令流请求 Agent 在 LVS Director 上调整 RS 权重（T035，摘除式灰度），阻塞等待结果。
func (s *Server) SetRSWeight(ctx context.Context, nodeID int, task *agentv1.SetRealServerWeightTask) (*agentv1.SetRealServerWeightResult, error) {
	taskID := ensureTaskID(task.GetTaskId())
	task.TaskId = taskID
	cmd := &agentv1.HeartbeatResponse{
		Command:    agentv1.HeartbeatResponse_SET_RS_WEIGHT,
		TaskId:     taskID,
		SetRsWeight: task,
	}
	return s.waitRSWeightResult(ctx, nodeID, cmd, rsWeightTimeout)
}

// DeployCert 经心跳命令流请求目标 Agent 原子落盘证书（T044），阻塞等待终态 DeployCertResult。
func (s *Server) DeployCert(ctx context.Context, nodeID int, task *agentv1.DeployCertTask) (*agentv1.DeployCertResult, error) {
	taskID := ensureTaskID(task.GetTaskId())
	task.TaskId = taskID
	cmd := &agentv1.HeartbeatResponse{
		Command:    agentv1.HeartbeatResponse_DEPLOY_CERT,
		TaskId:     taskID,
		DeployCert: task,
	}
	return s.waitCertResult(ctx, nodeID, cmd, certTimeout)
}

// waitCertResult 注册按 task_id 匹配的 DeployCertResult 通道，下发命令并阻塞等待结果。
func (s *Server) waitCertResult(ctx context.Context, nodeID int, cmd *agentv1.HeartbeatResponse, timeout time.Duration) (*agentv1.DeployCertResult, error) {
	taskID := cmd.TaskId
	ch := make(chan *agentv1.DeployCertResult, 1)
	s.cmdMu.Lock()
	s.certChans[taskID] = ch
	s.cmdMu.Unlock()
	defer func() {
		s.cmdMu.Lock()
		delete(s.certChans, taskID)
		s.cmdMu.Unlock()
	}()
	if err := s.sendCmd(nodeID, cmd); err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, apperr.New(apperr.CodeUnavailable, "证书分发请求被取消").WithDetail(ctx.Err().Error())
	case <-time.After(timeout):
		return nil, apperr.New(apperr.CodeUnavailable, "证书分发超时（Agent 未回传结果）")
	}
}

// sendCmd 经会话命令通道下发一条心跳指令，会话满时少量重试；离线/不可达返回 apperr。
func (s *Server) sendCmd(nodeID int, cmd *agentv1.HeartbeatResponse) error {
	var sendErr error
	for i := 0; i < 3; i++ {
		sendErr = s.sessions.Send(nodeID, cmd)
		if sendErr == nil {
			return nil
		}
		if errors.Is(sendErr, session.ErrSessionNotFound) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if errors.Is(sendErr, session.ErrSessionNotFound) {
		return apperr.New(apperr.CodeUnavailable, "目标 Agent 未在线，无法下发指令")
	}
	return apperr.New(apperr.CodeUnavailable, "下发指令失败").WithDetail(sendErr.Error())
}

// waitDeployResult 注册按 task_id 匹配的 DeployProgress 通道，下发命令并阻塞等待终态。
func (s *Server) waitDeployResult(ctx context.Context, nodeID int, cmd *agentv1.HeartbeatResponse, timeout time.Duration) (*agentv1.DeployProgress, error) {
	taskID := cmd.TaskId
	ch := make(chan *agentv1.DeployProgress, 1)
	s.cmdMu.Lock()
	s.deployChans[taskID] = ch
	s.cmdMu.Unlock()
	defer func() {
		s.cmdMu.Lock()
		delete(s.deployChans, taskID)
		s.cmdMu.Unlock()
	}()
	if err := s.sendCmd(nodeID, cmd); err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, apperr.New(apperr.CodeUnavailable, "部署请求被取消").WithDetail(ctx.Err().Error())
	case <-time.After(timeout):
		return nil, apperr.New(apperr.CodeUnavailable, "部署超时（Agent 未回传结果）")
	}
}

// waitSnapshotResult 注册按 task_id 匹配的 SnapshotResult 通道，下发命令并阻塞等待结果。
func (s *Server) waitSnapshotResult(ctx context.Context, nodeID int, cmd *agentv1.HeartbeatResponse, timeout time.Duration) (*agentv1.SnapshotResult, error) {
	taskID := cmd.TaskId
	ch := make(chan *agentv1.SnapshotResult, 1)
	s.cmdMu.Lock()
	s.snapshotChans[taskID] = ch
	s.cmdMu.Unlock()
	defer func() {
		s.cmdMu.Lock()
		delete(s.snapshotChans, taskID)
		s.cmdMu.Unlock()
	}()
	if err := s.sendCmd(nodeID, cmd); err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, apperr.New(apperr.CodeUnavailable, "快照请求被取消").WithDetail(ctx.Err().Error())
	case <-time.After(timeout):
		return nil, apperr.New(apperr.CodeUnavailable, "快照超时（Agent 未回传结果）")
	}
}

// waitRSWeightResult 注册按 task_id 匹配的 SetRealServerWeightResult 通道，下发命令并阻塞等待结果。
func (s *Server) waitRSWeightResult(ctx context.Context, nodeID int, cmd *agentv1.HeartbeatResponse, timeout time.Duration) (*agentv1.SetRealServerWeightResult, error) {
	taskID := cmd.TaskId
	ch := make(chan *agentv1.SetRealServerWeightResult, 1)
	s.cmdMu.Lock()
	s.rsWeightChans[taskID] = ch
	s.cmdMu.Unlock()
	defer func() {
		s.cmdMu.Lock()
		delete(s.rsWeightChans, taskID)
		s.cmdMu.Unlock()
	}()
	if err := s.sendCmd(nodeID, cmd); err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, apperr.New(apperr.CodeUnavailable, "调权请求被取消").WithDetail(ctx.Err().Error())
	case <-time.After(timeout):
		return nil, apperr.New(apperr.CodeUnavailable, "调权超时（Agent 未回传结果）")
	}
}

// deliverDeployResult 把 Agent 回传的部署/回滚结果投递给等待中的请求（按 task_id 匹配）。
func (s *Server) deliverDeployResult(taskID string, res *agentv1.DeployProgress) {
	if taskID == "" {
		return
	}
	s.cmdMu.Lock()
	ch, ok := s.deployChans[taskID]
	if ok {
		delete(s.deployChans, taskID)
	}
	s.cmdMu.Unlock()
	if ok {
		select {
		case ch <- res:
		default:
		}
	}
}

// deliverSnapshotResult 把 Agent 回传的快照结果投递给等待中的请求（按 task_id 匹配）。
func (s *Server) deliverSnapshotResult(taskID string, res *agentv1.SnapshotResult) {
	if taskID == "" {
		return
	}
	s.cmdMu.Lock()
	ch, ok := s.snapshotChans[taskID]
	if ok {
		delete(s.snapshotChans, taskID)
	}
	s.cmdMu.Unlock()
	if ok {
		select {
		case ch <- res:
		default:
		}
	}
}

// deliverRSWeightResult 把 Agent 回传的调权结果投递给等待中的请求（按 task_id 匹配）。
func (s *Server) deliverRSWeightResult(taskID string, res *agentv1.SetRealServerWeightResult) {
	if taskID == "" {
		return
	}
	s.cmdMu.Lock()
	ch, ok := s.rsWeightChans[taskID]
	if ok {
		delete(s.rsWeightChans, taskID)
	}
	s.cmdMu.Unlock()
	if ok {
		select {
		case ch <- res:
		default:
		}
	}
}

// deliverCertResult 把 Agent 回传的证书落盘结果投递给等待中的请求（按 task_id 匹配）。
func (s *Server) deliverCertResult(taskID string, res *agentv1.DeployCertResult) {
	if taskID == "" {
		return
	}
	s.cmdMu.Lock()
	ch, ok := s.certChans[taskID]
	if ok {
		delete(s.certChans, taskID)
	}
	s.cmdMu.Unlock()
	if ok {
		select {
		case ch <- res:
		default:
		}
	}
}

// ensureTaskID 返回非空 task_id，为空则生成随机幂等键。
func ensureTaskID(id string) string {
	if id != "" {
		return id
	}
	return newTaskID()
}
