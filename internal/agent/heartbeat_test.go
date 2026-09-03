// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 tianhao

package agent_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent"
	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/agent/transport"
	"github.com/th/ngxcp/internal/pkg/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// fakeEnroll 仅用于起测试 gRPC 服务（心跳测试不触碰注册逻辑）。
type fakeEnroll struct{}

func (f *fakeEnroll) VerifyEnrollToken(_ context.Context, _ string) (int, error) {
	return 0, fmt.Errorf("unused in heartbeat test")
}
func (f *fakeEnroll) MarkEnrolled(_ context.Context, _ int) error { return nil }

func genCSRPEM(t *testing.T, hostname string, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	tmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func clientCredsForNode(t *testing.T, ca *pki.CA, nodeID int, hostname string, key *ecdsa.PrivateKey) credentials.TransportCredentials {
	t.Helper()
	csrPEM := genCSRPEM(t, hostname, key)
	_, certPEM, err := ca.IssueAgentCert(csrPEM, nodeID, hostname, time.Hour)
	if err != nil {
		t.Fatalf("签发客户端证书: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("无法 PEM 解码证书")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CACertPEM()) {
		t.Fatal("无法将 CA 加入信任池")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{block.Bytes}, PrivateKey: key}},
		RootCAs:      pool,
		ServerName:   pki.ServerCommonName(),
	})
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("等待条件超时")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHeartbeaterConnectsAndRefreshes 验证 Agent 侧心跳客户端：
//  1. 启动后控制面会话管理看到该节点在线，并记录时钟偏差；
//  2. 控制面下发 REFRESH_CAPABILITY 指令，客户端 ReportCapability 回调被触发。
func TestHeartbeaterConnectsAndRefreshes(t *testing.T) {
	ca, _ := pki.LoadOrCreateCA(t.TempDir())
	sessions := session.NewSessionManager(nil)
	srv := transport.NewServer(nil, ca, &fakeEnroll{}, nil, sessions,
		session.HeartbeatConfig{Timeout: 30 * time.Second, ClockSkewWarn: 1 * time.Second})
	tlsCfg, _ := ca.GRPCServerTLSConfig()
	g := srv.BuildGRPCServer(tlsCfg)
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()

	const nodeID = 11
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(clientCredsForNode(t, ca, nodeID, "rs-11", key)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	cli := agentv1.NewAgentServiceClient(conn)

	var mu sync.Mutex
	refreshed := false
	hb := agent.NewHeartbeater(cli,
		agent.HeartbeatConfig{Interval: 150 * time.Millisecond, ReconnectBase: time.Second, ReconnectMax: 5 * time.Second},
		agent.HeartbeatCallbacks{
			ReportCapability: func(ctx context.Context) error {
				mu.Lock()
				refreshed = true
				mu.Unlock()
				return nil
			},
		}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hb.Run(ctx) }()

	waitUntil(t, 2*time.Second, func() bool { return sessions.IsOnline(nodeID) })
	if sk, ok := sessions.ClockSkewSeconds(nodeID); !ok || sk > 2 || sk < -2 {
		t.Errorf("时钟偏差 = %v (ok=%v)，期望在 ±2s 内", sk, ok)
	}

	// 控制面下发刷新指令 → 客户端回调应被触发。
	if err := sessions.Send(nodeID, &agentv1.HeartbeatResponse{Command: agentv1.HeartbeatResponse_REFRESH_CAPABILITY}); err != nil {
		t.Fatalf("下发指令: %v", err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return refreshed
	})
}

// TestHeartbeaterReconnect 验证断线后能在退避后重连并再次上线（指数退避，不panic）。
func TestHeartbeaterReconnect(t *testing.T) {
	ca, _ := pki.LoadOrCreateCA(t.TempDir())
	sessions := session.NewSessionManager(nil)
	srv := transport.NewServer(nil, ca, &fakeEnroll{}, nil, sessions, session.HeartbeatConfig{Timeout: 30 * time.Second})
	tlsCfg, _ := ca.GRPCServerTLSConfig()
	g := srv.BuildGRPCServer(tlsCfg)
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()

	const nodeID = 12
	addr := lis.Addr().String()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(clientCredsForNode(t, ca, nodeID, "rs-12", key)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	cli := agentv1.NewAgentServiceClient(conn)

	hb := agent.NewHeartbeater(cli,
		agent.HeartbeatConfig{Interval: 100 * time.Millisecond, ReconnectBase: 200 * time.Millisecond, ReconnectMax: time.Second},
		agent.HeartbeatCallbacks{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hb.Run(ctx) }()

	// 首轮上线
	waitUntil(t, 2*time.Second, func() bool { return sessions.IsOnline(nodeID) })

	// 杀掉 gRPC server（并关闭监听端口），客户端应断开
	g.Stop()
	_ = lis.Close()
	waitUntil(t, 3*time.Second, func() bool { return !sessions.IsOnline(nodeID) })

	// 在原地址重启 server，客户端应在退避后重连上线
	time.Sleep(100 * time.Millisecond)
	g2 := srv.BuildGRPCServer(tlsCfg)
	lis2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("re-listen: %v", err)
	}
	go func() { _ = g2.Serve(lis2) }()
	defer g2.Stop()
	waitUntil(t, 5*time.Second, func() bool { return sessions.IsOnline(nodeID) })
}

// ---- 以下为 Agent 侧健康探测上报链路测试（fake gRPC client 捕获流上发送的请求）----

// recordingStream 是一个内存化的心跳流实现，记录 Send 出去的请求并回放预置指令。
type recordingStream struct {
	ctx  context.Context
	sent chan *agentv1.HeartbeatRequest
	cmds chan *agentv1.HeartbeatResponse
}

func (s *recordingStream) Send(r *agentv1.HeartbeatRequest) error { s.sent <- r; return nil }
func (s *recordingStream) Recv() (*agentv1.HeartbeatResponse, error) {
	select {
	case c, ok := <-s.cmds:
		if !ok {
			return nil, io.EOF
		}
		return c, nil
	case <-s.ctx.Done():
		return nil, io.EOF
	}
}
func (s *recordingStream) CloseSend() error            { return nil }
func (s *recordingStream) Context() context.Context    { return s.ctx }
func (s *recordingStream) Header() (metadata.MD, error) { return nil, nil }
func (s *recordingStream) Trailer() metadata.MD         { return nil }
func (s *recordingStream) SendMsg(m any) error          { return nil }
func (s *recordingStream) RecvMsg(m any) error          { return nil }

// fakeAgentClient 实现 agentv1.AgentServiceClient，心跳流复用同一个 recordingStream。
type fakeAgentClient struct {
	stream *recordingStream
}

func (c *fakeAgentClient) Register(_ context.Context, _ *agentv1.RegisterRequest, _ ...grpc.CallOption) (*agentv1.RegisterResponse, error) {
	return &agentv1.RegisterResponse{}, nil
}
func (c *fakeAgentClient) Heartbeat(_ context.Context, _ ...grpc.CallOption) (agentv1.AgentService_HeartbeatClient, error) {
	return c.stream, nil
}
func (c *fakeAgentClient) ReportCapability(_ context.Context, _ *agentv1.CapabilityReport, _ ...grpc.CallOption) (*agentv1.Ack, error) {
	return &agentv1.Ack{Ok: true}, nil
}

// TestHeartbeaterReportsComplianceAndFsProbe 验证 Agent 侧探测执行器结果经心跳流上行：
//   - 首发 PING；
//   - 周期运行 FS 健康探测并上报 FS_PROBE；
//   - 收到 RUN_COMPLIANCE 指令后运行 DR 合规自检并上报 COMPLIANCE。
func TestHeartbeaterReportsComplianceAndFsProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &recordingStream{
		ctx:  ctx,
		sent: make(chan *agentv1.HeartbeatRequest, 64),
		cmds: make(chan *agentv1.HeartbeatResponse, 8),
	}
	cli := &fakeAgentClient{stream: stream}

	// 200ms 后下发一次 RUN_COMPLIANCE 指令，触发合规上报。
	go func() {
		time.Sleep(200 * time.Millisecond)
		stream.cmds <- &agentv1.HeartbeatResponse{Command: agentv1.HeartbeatResponse_RUN_COMPLIANCE}
	}()

	hb := agent.NewHeartbeater(cli,
		agent.HeartbeatConfig{Interval: 100 * time.Millisecond, FsProbeInterval: 150 * time.Millisecond},
		agent.HeartbeatCallbacks{
			ReportCompliance: func(ctx context.Context) (*agentv1.ComplianceReport, error) {
				return &agentv1.ComplianceReport{
					CheckedAt: time.Now().Unix(),
					Items:     []*agentv1.ComplianceItem{{Name: "vip_on_lo", Passed: true}},
				}, nil
			},
			ReportFsProbe: func(ctx context.Context) (*agentv1.FsProbeReport, error) {
				return &agentv1.FsProbeReport{
					CheckedAt: time.Now().Unix(),
					Items:     []*agentv1.ComplianceItem{{Name: "disk_usage_nginx_paths", Passed: true}},
				}, nil
			},
		}, nil)

	go func() { _ = hb.Run(ctx) }()

	seen := map[agentv1.HeartbeatRequest_Type]bool{}
	timeout := time.After(3 * time.Second)
	for len(seen) < 3 { // 期望观察 PING + COMPLIANCE + FS_PROBE
		select {
		case r := <-stream.sent:
			seen[r.GetType()] = true
		case <-timeout:
			t.Fatalf("未在超时内观察到全部上报类型，已见: %v", seen)
		}
	}
	if !seen[agentv1.HeartbeatRequest_PING] {
		t.Error("未见 PING 上报")
	}
	if !seen[agentv1.HeartbeatRequest_COMPLIANCE] {
		t.Error("未见 COMPLIANCE 上报（DR 合规自检未上行）")
	}
	if !seen[agentv1.HeartbeatRequest_FS_PROBE] {
		t.Error("未见 FS_PROBE 上报（日志/FS 健康探测未上行）")
	}
}
