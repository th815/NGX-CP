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
//  2. 控制面下发 REFRESH_CAPABILITY 指令，客户端 reportCap 回调被触发。
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
		func(ctx context.Context) error {
			mu.Lock()
			refreshed = true
			mu.Unlock()
			return nil
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
		nil, nil)
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
