// Package transport 实现控制面 ↔ Agent 的 gRPC 传输层。
// 本文件为 T011 骨架：完成服务装配，具体业务（注册 / PKI / 心跳）由 T012/T014/T015 注入。
package transport

import (
	"context"
	"log/slog"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server 实现 agentv1.AgentServiceServer。
// 未实现的方法通过嵌入 UnimplementedAgentServiceServer 返回 CodeUnimplemented，
// 保证传输层可独立编译与启动，业务方法后续里程碑逐个落地。
type Server struct {
	agentv1.UnimplementedAgentServiceServer
	log *slog.Logger
}

// NewServer 构造 gRPC 服务端骨架。
func NewServer(log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{log: log}
}

// Register 注册逻辑（T014 落地，依赖 T012 PKI）。
func (s *Server) Register(_ context.Context, _ *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	return nil, status.Error(codes.Unimplemented, "注册逻辑将在 T014 接入（依赖 T012 PKI）")
}

// ReportCapability 能力上报落库（T015 落地）。
func (s *Server) ReportCapability(_ context.Context, _ *agentv1.CapabilityReport) (*agentv1.Ack, error) {
	return nil, status.Error(codes.Unimplemented, "能力上报落库将在 T015 接入")
}

// Heartbeat 双向流会话管理（T015 落地）。
func (s *Server) Heartbeat(_ agentv1.AgentService_HeartbeatServer) error {
	return status.Error(codes.Unimplemented, "心跳会话管理将在 T015 接入")
}

// Attach 将本服务注册到 grpc.Server。
func (s *Server) Attach(g *grpc.Server) {
	agentv1.RegisterAgentServiceServer(g, s)
}
