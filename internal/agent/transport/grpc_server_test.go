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
	"github.com/th/ngxcp/ent"
	entnode "github.com/th/ngxcp/ent/node"
	entnodecap "github.com/th/ngxcp/ent/nodecapability"
	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/pkg/pki"
	"github.com/th/ngxcp/internal/repo"
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
// 用于纯注册单测：心跳/落库相关依赖传 nil / 零值即可（Register 不触碰它们）。
func startTestServer(t *testing.T, ca *pki.CA, enroll EnrollBackend) (string, func()) {
	t.Helper()
	tlsCfg, err := ca.GRPCServerTLSConfig()
	if err != nil {
		t.Fatalf("grpc tls config: %v", err)
	}
	srv := NewServer(slog.Default(), ca, enroll, nil, nil, session.HeartbeatConfig{})
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

// ---- T015：心跳与会话管理 ----

// pemKey 把 ECDSA 私钥编码为 PEM（供客户端 mTLS 凭证使用）。
func pemKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})
}

// clientCredsForNode 用控制面 CA 为指定 nodeID 签发客户端证书，构造 mTLS 客户端凭证。
func clientCredsForNode(t *testing.T, ca *pki.CA, nodeID int, hostname string, key *ecdsa.PrivateKey) credentials.TransportCredentials {
	t.Helper()
	csrPEM := genCSRPEM(t, hostname, key)
	_, certPEM, err := ca.IssueAgentCert(csrPEM, nodeID, hostname, time.Hour)
	if err != nil {
		t.Fatalf("签发客户端证书: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CACertPEM()) {
		t.Fatal("无法将 CA 加入信任池")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{mustDecode(t, certPEM)}, PrivateKey: key}},
		RootCAs:      pool,
		ServerName:   pki.ServerCommonName(),
	})
}

func mustDecode(t *testing.T, pemBytes []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("无法 PEM 解码证书")
	}
	return block.Bytes
}

// TestHeartbeatStream 验证：持客户端证书的 Agent 可建立心跳双向流；
// 控制面正确记录会话在线、时钟偏差，并在流断开后注销会话。
func TestHeartbeatStream(t *testing.T) {
	ca, _ := pki.LoadOrCreateCA(t.TempDir())
	sessions := session.NewSessionManager(slog.Default())
	// nodeSvc 传 nil：本测试只验证传输层 + 会话生命周期，不落库。
	srv := NewServer(slog.Default(), ca, &fakeEnroll{}, nil, sessions,
		session.HeartbeatConfig{Timeout: 30 * time.Second, ClockSkewWarn: 1 * time.Second})
	tlsCfg, _ := ca.GRPCServerTLSConfig()
	g := srv.BuildGRPCServer(tlsCfg)
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()

	const nodeID = 9
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(clientCredsForNode(t, ca, nodeID, "rs-09", key)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	cli := agentv1.NewAgentServiceClient(conn)

	hc, err := cli.Heartbeat(context.Background())
	if err != nil {
		t.Fatalf("打开心跳流: %v", err)
	}
	if err := hc.Send(&agentv1.HeartbeatRequest{
		Type:      agentv1.HeartbeatRequest_PING,
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("发送 PING: %v", err)
	}

	// 会话应在短时间内注册为在线。
	waitUntil(t, 2*time.Second, func() bool { return sessions.IsOnline(nodeID) })
	if sk, ok := sessions.ClockSkewSeconds(nodeID); !ok || sk > 2 || sk < -2 {
		t.Errorf("时钟偏差 = %v (ok=%v)，期望在 ±2s 内", sk, ok)
	}

	// 关闭流 → 会话应注销。
	_ = hc.CloseSend()
	waitUntil(t, 2*time.Second, func() bool { return !sessions.IsOnline(nodeID) })
}

// TestReportCapabilityPersists 验证：持 mTLS 证书的 Agent 上报能力基线后，
// 控制面落库到 node_capabilities 并按 FSM 将 enrolling → online。
func TestReportCapabilityPersists(t *testing.T) {
	ca, _ := pki.LoadOrCreateCA(t.TempDir())
	client, nodeID := newTestNode(t)
	defer client.Close()
	nodeSvc := node.New(client)
	sessions := session.NewSessionManager(slog.Default())
	srv := NewServer(slog.Default(), ca, &fakeEnroll{}, nodeSvc, sessions, session.HeartbeatConfig{})
	tlsCfg, _ := ca.GRPCServerTLSConfig()
	g := srv.BuildGRPCServer(tlsCfg)
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(clientCredsForNode(t, ca, nodeID, "rs-09", key)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	cli := agentv1.NewAgentServiceClient(conn)

	_, err = cli.ReportCapability(context.Background(), &agentv1.CapabilityReport{
		Capability: &agentv1.Capability{
			Hostname: "rs-09",
			Nginx: &agentv1.NginxInfo{
				Version:       "1.30.0",
				Prefix:        "/etc/nginx",
				ConfPath:      "/etc/nginx/nginx.conf",
				SbinPath:      "/usr/sbin/nginx",
				StaticModules: []string{"http_ssl", "stream", "http_v2"},
				ConfigureArgs: "--prefix=/etc/nginx --with-http_v3_module",
				ConfigHash:    "deadbeef",
			},
		},
	})
	if err != nil {
		t.Fatalf("ReportCapability: %v", err)
	}

	// 能力基线已落库。
	got, err := client.NodeCapability.Query().
		Where(entnodecap.HasNodeWith(entnode.ID(nodeID))).Only(context.Background())
	if err != nil {
		t.Fatalf("查询能力基线: %v", err)
	}
	if got.Version != "1.30.0" {
		t.Errorf("Version = %q, want 1.30.0", got.Version)
	}
	if len(got.Modules) != 3 || got.Modules[0] != "http_ssl" {
		t.Errorf("Modules = %v, want [http_ssl stream http_v2]", got.Modules)
	}

	// 节点由 enrolling 翻为 online。
	n, err := client.Node.Get(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("查询节点: %v", err)
	}
	if n.Status != entnode.StatusOnline {
		t.Errorf("节点状态 = %q, want online", n.Status)
	}
}

// newTestNode 起一个内存 sqlite + 自动建表，并创建一个 enrolling 态节点，返回 client 与节点 ID。
func newTestNode(t *testing.T) (*ent.Client, int) {
	t.Helper()
	client, err := repo.Open("sqlite", "file:ngxcp_t015?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("建表: %v", err)
	}
	n, err := client.Node.Create().
		SetName("rs-09").
		SetAddress("10.0.0.9").
		SetRole(entnode.RoleRealServer).
		SetStatus(entnode.StatusEnrolling).
		Save(context.Background())
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	return client, n.ID
}

// waitUntil 轮询 cond 直到为真或超时。
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
