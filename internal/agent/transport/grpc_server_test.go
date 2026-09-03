package transport

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
	"log/slog"
	"net"
	"testing"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/pkg/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// fakeEnroll 实现 EnrollBackend，供 T014 传输层测试（无需真实 DB）。
type fakeEnroll struct {
	token    string
	nodeID   int
	marked   bool
	markedID int
	markErr  error
}

func (f *fakeEnroll) VerifyEnrollToken(_ context.Context, raw string) (int, error) {
	if raw != f.token {
		return 0, fmt.Errorf("令牌无效")
	}
	return f.nodeID, nil
}

func (f *fakeEnroll) MarkEnrolled(_ context.Context, id int) error {
	f.marked = true
	f.markedID = id
	return f.markErr
}

// genCSRPEM 用给定私钥生成 CSR 的 PEM（Agent 本地生成密钥，私钥永不出节点）。
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

// startTestServer 起一个用 CA 的 GRPCServerTLSConfig 的 gRPC 服务，返回地址与停止函数。
func startTestServer(t *testing.T, ca *pki.CA, enroll EnrollBackend) (string, func()) {
	t.Helper()
	tlsCfg, err := ca.GRPCServerTLSConfig()
	if err != nil {
		t.Fatalf("grpc tls config: %v", err)
	}
	srv := NewServer(slog.Default(), ca, enroll)
	g := srv.BuildGRPCServer(tlsCfg)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = g.Serve(lis) }()
	return lis.Addr().String(), func() { g.Stop() }
}

// clientCreds 构造仅 TLS（无客户端证书）的凭证：信任控制面 CA，ServerName 匹配 SAN。
func clientCreds(t *testing.T, ca *pki.CA) credentials.TransportCredentials {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CACertPEM()) {
		t.Fatal("无法将 CA 证书加入信任池")
	}
	return credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: pki.ServerCommonName()})
}

// TestRegisterSuccess 验证：合法 token + CSR → 返回 NodeID / 客户端证书 / CA / 过期时间，并回写节点。
func TestRegisterSuccess(t *testing.T) {
	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	enroll := &fakeEnroll{token: "good-token", nodeID: 7}
	addr, stop := startTestServer(t, ca, enroll)
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCreds(t, ca)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	cli := agentv1.NewAgentServiceClient(conn)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	resp, err := cli.Register(context.Background(), &agentv1.RegisterRequest{
		EnrollToken: "good-token",
		Hostname:    "rs-01",
		Csr:         genCSRPEM(t, "rs-01", key),
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.NodeId != 7 {
		t.Errorf("NodeId = %d, want 7", resp.NodeId)
	}
	if len(resp.ClientCert) == 0 {
		t.Error("ClientCert 为空")
	}
	if len(resp.CaCert) == 0 {
		t.Error("CaCert 为空")
	}
	if resp.CertExpiresAt <= time.Now().Unix() {
		t.Error("CertExpiresAt 不在未来")
	}
	if !enroll.marked || enroll.markedID != 7 {
		t.Error("MarkEnrolled 未以正确 nodeID 调用")
	}

	// 返回的客户端证书必须可被解析，且 Serial == nodeID（零额外 token 反查身份）。
	block, _ := pem.Decode(resp.ClientCert)
	if block == nil {
		t.Fatal("无法 PEM 解码客户端证书")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse client cert: %v", err)
	}
	if cert.SerialNumber.Int64() != 7 {
		t.Errorf("证书 Serial = %d, want 7", cert.SerialNumber.Int64())
	}
	if len(cert.DNSNames) == 0 || cert.DNSNames[0] != "rs-01" {
		t.Errorf("证书 SAN 缺失: %v", cert.DNSNames)
	}
}

// TestRegisterBadToken 验证：非法 token → Unauthenticated。
func TestRegisterBadToken(t *testing.T) {
	ca, _ := pki.LoadOrCreateCA(t.TempDir())
	enroll := &fakeEnroll{token: "good-token", nodeID: 7}
	addr, stop := startTestServer(t, ca, enroll)
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCreds(t, ca)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	cli := agentv1.NewAgentServiceClient(conn)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, err = cli.Register(context.Background(), &agentv1.RegisterRequest{
		EnrollToken: "wrong-token",
		Hostname:    "rs-01",
		Csr:         genCSRPEM(t, "rs-01", key),
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("期望 Unauthenticated, 实际 %v", err)
	}
	if enroll.marked {
		t.Error("非法 token 不应触发 MarkEnrolled")
	}
}

// TestRegisterMissingFields 验证：缺字段 → InvalidArgument。
func TestRegisterMissingFields(t *testing.T) {
	ca, _ := pki.LoadOrCreateCA(t.TempDir())
	enroll := &fakeEnroll{token: "good-token", nodeID: 7}
	addr, stop := startTestServer(t, ca, enroll)
	defer stop()

	conn, _ := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCreds(t, ca)))
	defer conn.Close()
	cli := agentv1.NewAgentServiceClient(conn)

	_, err := cli.Register(context.Background(), &agentv1.RegisterRequest{Hostname: "rs-01"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("期望 InvalidArgument, 实际 %v", err)
	}
}

// TestHeartbeatRequiresMTLSCert 验证：非 Register 的 RPC 在无客户端证书时被拦截器拒绝（Unauthenticated）。
// 证明 mTLS 强制生效，身份只能来自证书 Serial。
func TestHeartbeatRequiresMTLSCert(t *testing.T) {
	ca, _ := pki.LoadOrCreateCA(t.TempDir())
	enroll := &fakeEnroll{token: "good-token", nodeID: 7}
	addr, stop := startTestServer(t, ca, enroll)
	defer stop()

	conn, _ := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCreds(t, ca)))
	defer conn.Close()
	cli := agentv1.NewAgentServiceClient(conn)

	// 无客户端证书调用 ReportCapability（应在 UnaryAuth 拦截器处被拒，而非到达 Unimplemented 处理器）。
	_, err := cli.ReportCapability(context.Background(), &agentv1.CapabilityReport{NodeId: 7})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("期望 Unauthenticated, 实际 %v (拦截器未生效)", err)
	}
}
