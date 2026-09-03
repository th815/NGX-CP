// Package transport 实现控制面 ↔ Agent 的 gRPC 传输层。
// 本文件落地 T014 注册逻辑，并定义 per-RPC 鉴权拦截器：
//   - Register：Agent 尚无客户端证书，走 TLS + enroll token 鉴权（拦截器放宽客户端证书要求）。
//   - Heartbeat / ReportCapability：强制 mTLS 客户端证书，身份由证书 Serial 反查 nodeID。
package transport

import (
	"context"
	"crypto/tls"
	"log/slog"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/pkg/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// EnrollBackend 是注册流程所需的节点后端能力（由控制面 node.Service 实现）。
// 用接口隔离传输层与域逻辑，便于单测用 fake 替代。
type EnrollBackend interface {
	// VerifyEnrollToken 校验一次性接入令牌，返回其绑定的 nodeID。
	VerifyEnrollToken(ctx context.Context, raw string) (nodeID int, err error)
	// MarkEnrolled 将节点标记为已注册（enrolling → online）。
	MarkEnrolled(ctx context.Context, nodeID int) error
}

// enrollCertTTL Agent 注册后签发的客户端证书有效期（默认 1 年，到期前由控制面触发续签）。
const enrollCertTTL = 365 * 24 * time.Hour

// defaultServerConfig 注册响应下发的采集/心跳配置（与 proto 默认值一致）。
func defaultServerConfig() *agentv1.ServerConfig {
	return &agentv1.ServerConfig{
		HeartbeatIntervalSec: 10,
		HeartbeatTimeoutSec:  30,
		ClockSkewWarnSec:     1,
	}
}

// Server 实现 agentv1.AgentServiceServer。
type Server struct {
	agentv1.UnimplementedAgentServiceServer
	log          *slog.Logger
	ca           *pki.CA
	enroll       EnrollBackend
	serverConfig *agentv1.ServerConfig
}

// NewServer 构造 gRPC 服务端。ca 用于签发 Agent 客户端证书；enroll 用于校验接入令牌并回绑节点。
func NewServer(log *slog.Logger, ca *pki.CA, enroll EnrollBackend) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		log:          log,
		ca:           ca,
		enroll:       enroll,
		serverConfig: defaultServerConfig(),
	}
}

// Register 实现注册 RPC：Agent 用一次性 enroll token + 本地生成的 CSR 换取 mTLS 客户端证书。
// 此阶段 Agent 尚无客户端证书，故走 TLS+token 鉴权（由 UnaryAuth 拦截器对 Register 放宽客户端证书要求）。
func (s *Server) Register(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	if req.GetEnrollToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "缺少 enroll_token")
	}
	if req.GetHostname() == "" {
		return nil, status.Error(codes.InvalidArgument, "缺少 hostname")
	}
	if len(req.GetCsr()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "缺少 csr")
	}

	// 令牌校验（一次性）：存在 + 未使用 + 未过期，成功则标记已用，返回绑定的 nodeID。
	nodeID, err := s.enroll.VerifyEnrollToken(ctx, req.GetEnrollToken())
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "令牌校验失败: "+err.Error())
	}

	// 用 Agent 提交的 CSR 签发客户端证书：Serial=nodeID（零额外 token 反查身份），SAN=hostname。
	cert, certPEM, err := s.ca.IssueAgentCert(req.GetCsr(), nodeID, req.GetHostname(), enrollCertTTL)
	if err != nil {
		return nil, status.Error(codes.Internal, "签发客户端证书失败: "+err.Error())
	}

	// 回写节点状态 enrolling → online。
	if err := s.enroll.MarkEnrolled(ctx, nodeID); err != nil {
		return nil, status.Error(codes.Internal, "回写节点状态失败: "+err.Error())
	}

	s.log.Info("agent registered", "node_id", nodeID, "hostname", req.GetHostname())
	return &agentv1.RegisterResponse{
		NodeId:        int64(nodeID),
		ClientCert:    certPEM,
		CaCert:        s.ca.CACertPEM(),
		CertExpiresAt: cert.NotAfter.Unix(),
		Config:        s.serverConfig,
	}, nil
}

// ReportCapability 能力上报落库（T015 落地）。
func (s *Server) ReportCapability(_ context.Context, _ *agentv1.CapabilityReport) (*agentv1.Ack, error) {
	return nil, status.Error(codes.Unimplemented, "能力上报落库将在 T015 接入")
}

// Heartbeat 双向流会话管理（T015 落地）。
func (s *Server) Heartbeat(_ agentv1.AgentService_HeartbeatServer) error {
	return status.Error(codes.Unimplemented, "心跳会话管理将在 T015 接入")
}

// NodeIDFromContext 从已验证的 mTLS 对端证书 Serial 反查 nodeID。
// 供 T015 的 Heartbeat / ReportCapability 业务直接使用（身份已由拦截器保证）。
func NodeIDFromContext(ctx context.Context) (int, error) {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil || p.AuthInfo == nil {
		return 0, status.Error(codes.Unauthenticated, "缺少客户端证书")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return 0, status.Error(codes.Unauthenticated, "非 TLS 连接")
	}
	certs := tlsInfo.State.PeerCertificates
	if len(certs) == 0 {
		return 0, status.Error(codes.Unauthenticated, "缺少客户端证书")
	}
	return int(certs[0].SerialNumber.Int64()), nil
}

// UnaryAuth 一元拦截器：Register 仅用 token 鉴权；其余 RPC 强制 mTLS 客户端证书。
func (s *Server) UnaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if info.FullMethod == "/agent.v1.AgentService/Register" {
		return handler(ctx, req)
	}
	if _, err := NodeIDFromContext(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// StreamAuth 流式拦截器：当前仅 Heartbeat；强制 mTLS 客户端证书。
func (s *Server) StreamAuth(_ any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if info.FullMethod == "/agent.v1.AgentService/Heartbeat" {
		if _, err := NodeIDFromContext(ss.Context()); err != nil {
			return err
		}
	}
	return handler(nil, ss)
}

// Attach 将本服务注册到 grpc.Server。
func (s *Server) Attach(g *grpc.Server) {
	agentv1.RegisterAgentServiceServer(g, s)
}

// BuildGRPCServer 构造已装配本服务与双拦截器的 grpc.Server（需传入 gRPC 服务端 TLS 配置）。
// 调用方通常用 ca.GRPCServerTLSConfig()（VerifyClientCertIfGiven：注册期不强制客户端证书）。
func (s *Server) BuildGRPCServer(tlsConfig *tls.Config) *grpc.Server {
	g := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.UnaryInterceptor(s.UnaryAuth),
		grpc.StreamInterceptor(s.StreamAuth),
	)
	s.Attach(g)
	return g
}
