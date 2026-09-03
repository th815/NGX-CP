// Package transport 实现控制面 ↔ Agent 的 gRPC 传输层。
// 本文件落地 T014 注册逻辑，并定义 per-RPC 鉴权拦截器：
//   - Register：Agent 尚无客户端证书，走 TLS + enroll token 鉴权（拦截器放宽客户端证书要求）。
//   - Heartbeat / ReportCapability：强制 mTLS 客户端证书，身份由证书 Serial 反查 nodeID。
package transport

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/pkg/apperr"
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

// Server 实现 agentv1.AgentServiceServer。
type Server struct {
	agentv1.UnimplementedAgentServiceServer
	log          *slog.Logger
	ca           *pki.CA
	enroll       EnrollBackend
	nodeSvc      *node.Service      // 心跳落库 / 状态机（T015）
	sessions     *session.SessionManager // 会话管理（T015）
	hbCfg        session.HeartbeatConfig  // 心跳参数（T015）
	serverConfig *agentv1.ServerConfig

	// validateMu / validateChans 是 T024 校验结果回传的协调结构：
	// 控制面下发 VALIDATE_CONFIG 命令时注册一个按 task_id 匹配的接收通道，
	// Agent 经心跳流回传 CONFIG_VALIDATE 结果后由 Heartbeat 循环投递到此通道。
	validateMu    sync.Mutex
	validateChans map[string]chan *agentv1.ValidateResult
}

// NewServer 构造 gRPC 服务端。
// ca 用于签发 Agent 客户端证书；enroll 用于校验接入令牌并回绑节点；
// nodeSvc / sessions / hbCfg 为 T015 心跳与会话管理所需（可传 nil 以满足纯注册单测）。
func NewServer(log *slog.Logger, ca *pki.CA, enroll EnrollBackend, nodeSvc *node.Service, sessions *session.SessionManager, hbCfg session.HeartbeatConfig) *Server {
	if log == nil {
		log = slog.Default()
	}
	sc := &agentv1.ServerConfig{
		HeartbeatIntervalSec: 10,
		HeartbeatTimeoutSec:  30,
		ClockSkewWarnSec:     1,
	}
	if hbCfg.Interval > 0 {
		sc.HeartbeatIntervalSec = int64(hbCfg.Interval.Seconds())
	}
	if hbCfg.Timeout > 0 {
		sc.HeartbeatTimeoutSec = int64(hbCfg.Timeout.Seconds())
	}
	if hbCfg.ClockSkewWarn > 0 {
		sc.ClockSkewWarnSec = int64(hbCfg.ClockSkewWarn.Seconds())
	}
	return &Server{
		log:          log,
		ca:           ca,
		enroll:       enroll,
		nodeSvc:      nodeSvc,
		sessions:     sessions,
		hbCfg:        hbCfg,
		serverConfig: sc,

		validateChans: make(map[string]chan *agentv1.ValidateResult),
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

// ReportCapability 能力上报落库（T016/T017 解析结果经 Agent 采集后入库）。
// 身份来自 mTLS 证书 Serial（NodeIDFromContext），proto 里的 node_id 不可信，直接忽略。
// 上报成功后按 FSM 将 enrolling → online。
func (s *Server) ReportCapability(ctx context.Context, req *agentv1.CapabilityReport) (*agentv1.Ack, error) {
	nodeID, err := NodeIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	cap := req.GetCapability()
	if cap == nil {
		return nil, status.Error(codes.InvalidArgument, "缺少 capability")
	}
	if s.nodeSvc == nil {
		return nil, status.Error(codes.Internal, "节点服务未注入")
	}
	in := node.CapabilityIn{
		Hostname:      cap.GetHostname(),
		OS:            cap.GetOs(),
		Kernel:        cap.GetKernel(),
		HasKeepalived: cap.GetHasKeepalived(),
		HasIPVS:       cap.GetHasIpvsadm(),
	}
	if ng := cap.GetNginx(); ng != nil {
		in.NginxVersion = ng.GetVersion()
		in.NginxPrefix = ng.GetPrefix()
		in.NginxConfPath = ng.GetConfPath()
		in.NginxSbinPath = ng.GetSbinPath()
		in.NginxModules = ng.GetStaticModules()
		in.NginxRawArgs = ng.GetConfigureArgs()
		in.ConfigHash = ng.GetConfigHash()
	}
	// 主机运行底座画像（T018）：尽力而为采集，缺失不阻断能力上报。
	if sys := cap.GetSystem(); sys != nil {
		in.SystemInfo = &node.SystemInfoView{
			OS:             sys.GetOs(),
			Kernel:         sys.GetKernel(),
			NginxManagedBy: sys.GetNginxManagedBy(),
			SELinuxStatus:  sys.GetSelinuxStatus(),
			UlimitNofile:   int(sys.GetUlimitNofile()),
			Timezone:       sys.GetTimezone(),
			NTPSynced:      sys.GetNtpSynced(),
			LogRotateConf:  sys.GetLogrotateConf(),
			DiskFree:       sys.GetDiskFree(),
			Warnings:       sys.GetWarnings(),
		}
	}
	if err := s.nodeSvc.SaveCapability(ctx, nodeID, in); err != nil {
		return nil, status.Error(codes.Internal, "落库能力基线失败: "+err.Error())
	}
	s.log.Info("capability reported", "node_id", nodeID, "has_nginx", cap.GetNginx() != nil)
	return &agentv1.Ack{Ok: true}, nil
}

// Heartbeat 双向流：Agent 周期上报，控制面可随时经 CmdCh 下发指令（刷新能力 / 跑合规）。
//
// 并发与资源陷阱处理（见 docs/tasks/M1-skeleton.md T015）：
//   - 单写 goroutine 消费 CmdCh → stream.Send，杜绝 SendMsg 并发调用。
//   - 写入 goroutine 在 stream.Context().Done()（流断开）或 CmdCh 关闭时退出，无 goroutine 泄漏。
//   - 读循环 Recv 出错即 return，defer Unregister 清理会话，与写入 goroutine 解耦。
//   - 时钟偏差由控制面本地 now 与 Agent 上报 timestamp 之差计算，心跳超时同样用控制面本地时间。
func (s *Server) Heartbeat(stream agentv1.AgentService_HeartbeatServer) error {
	nodeID, err := NodeIDFromContext(stream.Context())
	if err != nil {
		return err
	}
	if s.sessions == nil {
		return status.Error(codes.Internal, "会话管理器未注入")
	}

	sess := &session.Session{
		NodeID:   nodeID,
		LastSeen: time.Now(),
		CmdCh:    make(chan *agentv1.HeartbeatResponse, 16),
	}
	s.sessions.Register(nodeID, sess)
	defer s.sessions.Unregister(nodeID)

	// 首跳即证明 Agent 存活：刷新心跳时间并翻转 enrolling/offline → online。
	if s.nodeSvc != nil {
		if err := s.nodeSvc.TouchHeartbeat(stream.Context(), nodeID); err != nil {
			s.log.Warn("heartbeat touch failed", "node_id", nodeID, "err", err)
		}
	}

	// 单写 goroutine：把会话指令下发到 Agent。
	go func() {
		for {
			select {
			case <-stream.Context().Done():
				return
			case cmd, ok := <-sess.CmdCh:
				if !ok {
					return
				}
				if err := stream.Send(cmd); err != nil {
					s.log.Debug("heartbeat command send failed", "node_id", nodeID, "err", err)
					return
				}
			}
		}
	}()

	// 读循环：接收心跳，刷新 LastSeen 与时钟偏差。
	for {
		req, err := stream.Recv()
		if err != nil {
			return err // 断开 → defer 清理会话
		}
		now := time.Now()
		s.sessions.UpdateLastSeen(nodeID, now)

		if ts := req.GetTimestamp(); ts > 0 {
			skew := now.Sub(time.Unix(ts, 0))
			s.sessions.SetClockSkew(nodeID, skew)
			if s.hbCfg.ClockSkewWarn > 0 && skew.Abs() > s.hbCfg.ClockSkewWarn {
				s.log.Warn("agent clock skew exceeds threshold",
					"node_id", nodeID, "skew_seconds", skew.Seconds())
				// TODO(T018): 产生一条 WARN 审计/告警事件供监控汇聚。
			}
		}

		// 合规自检上报（T019）：收到即缓存并判定，按 FSM 驱动 degraded / online。
		if c := req.GetCompliance(); c != nil && s.nodeSvc != nil {
			if err := s.nodeSvc.SetCompliance(stream.Context(), nodeID, c); err != nil {
				s.log.Warn("set compliance failed", "node_id", nodeID, "err", err)
			}
		}

		// 日志/FS 健康探测上报（T018）：收到即缓存并判定，与合规维度聚合驱动 degraded / online。
		if f := req.GetFsProbe(); f != nil && s.nodeSvc != nil {
			if err := s.nodeSvc.SetFsProbe(stream.Context(), nodeID, f); err != nil {
				s.log.Warn("set fs probe failed", "node_id", nodeID, "err", err)
			}
		}

		// 配置树上报（T018）：nginx -T 解析出的文件元数据快照，整体替换落库。
		if ct := req.GetConfigTree(); ct != nil && s.nodeSvc != nil {
			if err := s.nodeSvc.SaveConfigTree(stream.Context(), nodeID, ct.GetFiles()); err != nil {
				s.log.Warn("save config tree failed", "node_id", nodeID, "err", err)
			}
		}

		// 日志采集目标上报（T018）：决定 Agent 该 tail 哪些文件，整体替换落库。
		if lt := req.GetLogTargets(); lt != nil && s.nodeSvc != nil {
			if err := s.nodeSvc.SaveLogTargets(stream.Context(), nodeID, lt.GetItems()); err != nil {
				s.log.Warn("save log targets failed", "node_id", nodeID, "err", err)
			}
		}

		// 配置校验结果上报（T024）：按 task_id 投递给等待中的校验请求。
		if vr := req.GetValidateResult(); vr != nil {
			s.deliverValidateResult(vr.GetTaskId(), vr)
		}

		if s.nodeSvc != nil {
			_ = s.nodeSvc.TouchHeartbeat(stream.Context(), nodeID)
		}
	}
}

// validateTimeout 是单次校验请求等待 Agent 回传结果的最长阻塞时间。
const validateTimeout = 30 * time.Second

// RequestValidate 经心跳命令流请求目标 Agent 执行 nginx -t 校验，并阻塞等待结果回传。
// 这是 T024 控制面触发 Agent 校验的唯一入口（Agent 主动外连，控制面不主动连节点）。
// 实现要点：
//   - 生成/复用 task_id，注册按 task_id 匹配的接收通道；
//   - 经会话 CmdCh 下发 VALIDATE_CONFIG 命令（会话满则少量重试）；
//   - Agent 跑完 nginx -t 后经 CONFIG_VALIDATE 上报，Heartbeat 循环调 deliverValidateResult 投递；
//   - 超时/取消/离线均返回 apperr（便于 HTTP 层映射为 4103 / 4012）。
func (s *Server) ValidateConfig(ctx context.Context, nodeID int, task *agentv1.ValidateTask) (*agentv1.ValidateResult, error) {
	if task == nil {
		return nil, apperr.New(apperr.CodeInvalid, "校验任务为空")
	}
	taskID := task.GetTaskId()
	if taskID == "" {
		taskID = newTaskID()
		task.TaskId = taskID
	}

	ch := make(chan *agentv1.ValidateResult, 1)
	s.validateMu.Lock()
	s.validateChans[taskID] = ch
	s.validateMu.Unlock()
	defer func() {
		s.validateMu.Lock()
		delete(s.validateChans, taskID)
		s.validateMu.Unlock()
	}()

	cmd := &agentv1.HeartbeatResponse{
		Command:      agentv1.HeartbeatResponse_VALIDATE_CONFIG,
		TaskId:       taskID,
		ValidateTask: task,
	}

	// 下发命令：会话满（极端热路径）时少量重试，仍失败视为 Agent 不可达。
	var sendErr error
	for i := 0; i < 3; i++ {
		sendErr = s.sessions.Send(nodeID, cmd)
		if sendErr == nil {
			break
		}
		if errors.Is(sendErr, session.ErrSessionNotFound) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sendErr != nil {
		if errors.Is(sendErr, session.ErrSessionNotFound) {
			return nil, apperr.New(apperr.CodeUnavailable, "目标 Agent 未在线，无法执行校验")
		}
		return nil, apperr.New(apperr.CodeUnavailable, "下发校验指令失败").WithDetail(sendErr.Error())
	}

	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, apperr.New(apperr.CodeUnavailable, "校验请求被取消").WithDetail(ctx.Err().Error())
	case <-time.After(validateTimeout):
		return nil, apperr.New(apperr.CodeUnavailable, "校验超时（Agent 未回传结果）")
	}
}

// deliverValidateResult 把 Agent 回传的校验结果投递给等待中的请求（按 task_id 匹配）。
func (s *Server) deliverValidateResult(taskID string, res *agentv1.ValidateResult) {
	if taskID == "" {
		return
	}
	s.validateMu.Lock()
	ch, ok := s.validateChans[taskID]
	if ok {
		delete(s.validateChans, taskID)
	}
	s.validateMu.Unlock()
	if ok {
		select {
		case ch <- res:
		default:
		}
	}
}

// newTaskID 生成随机 task_id（幂等键），失败回退到时间戳。
func newTaskID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
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
// 注意：handler 的第一个参数必须是 gRPC 框架传入的 service 实现（srv），传 nil 会在
// 生成的 _Handler 里做接口断言时 panic。
func (s *Server) StreamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if info.FullMethod == "/agent.v1.AgentService/Heartbeat" {
		if _, err := NodeIDFromContext(ss.Context()); err != nil {
			return err
		}
	}
	return handler(srv, ss)
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
