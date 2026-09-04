package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	entnode "github.com/th/ngxcp/ent/node"
	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/pkg/pki"
	"github.com/th/ngxcp/internal/repo"
	"log/slog"
)

// TestRegisterSelfJoin 锁定「web 一键自注册」关键路径：
// 签发 Join Token → Agent 持 token + 本地 CSR 注册 → 控制面自动建节点（角色取令牌）
// → 签发客户端证书 → 节点上线（DB 中状态为 online）。
func TestRegisterSelfJoin(t *testing.T) {
	ctx := context.Background()
	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("ca: %v", err)
	}

	client, err := repo.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "join.db")+"?_fk=1")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer client.Close()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	nodeSvc := node.New(client, nil)
	// nodeSvc 自身即 EnrollBackend（VerifyEnrollToken/VerifyJoinToken/MarkEnrolled 均由它实现）。

	tlsCfg, err := ca.GRPCServerTLSConfig()
	if err != nil {
		t.Fatalf("grpc tls: %v", err)
	}
	srv := NewServer(slog.Default(), ca, nodeSvc, nodeSvc, nil, session.HeartbeatConfig{})
	g := srv.BuildGRPCServer(tlsCfg)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(clientCreds(t, ca)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	cli := agentv1.NewAgentServiceClient(conn)

	// 1) 签发 Join Token（real_server 角色）。
	joinTok, _, err := nodeSvc.IssueJoinToken(ctx, "real_server", time.Hour)
	if err != nil {
		t.Fatalf("issue join token: %v", err)
	}

	// 2) 持 Join Token + 本地 CSR 自注册。
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	resp, err := cli.Register(ctx, &agentv1.RegisterRequest{
		JoinToken: joinTok,
		Hostname:  "auto-rs-01",
		Csr:       genCSRPEM(t, "auto-rs-01", key),
	})
	if err != nil {
		t.Fatalf("register(join): %v", err)
	}
	if resp.NodeId == 0 {
		t.Fatal("自注册未分配 nodeID")
	}
	if len(resp.ClientCert) == 0 || len(resp.CaCert) == 0 {
		t.Fatal("自注册未返回证书")
	}

	// 3) 节点应已在 DB 中自动创建，角色与令牌一致，状态 online。
	n, err := client.Node.Query().Where(entnode.Name("auto-rs-01")).Only(ctx)
	if err != nil {
		t.Fatalf("查询自动创建的节点: %v", err)
	}
	if n.Role != entnode.RoleRealServer {
		t.Errorf("节点角色 = %q, want real_server", n.Role)
	}
	if n.Status != entnode.StatusOnline {
		t.Errorf("节点状态 = %q, want online（自注册后应直接上线）", n.Status)
	}

	// 4) Join Token 一次性：重复使用应失败。
	if _, err := cli.Register(ctx, &agentv1.RegisterRequest{
		JoinToken: joinTok, Hostname: "dup", Csr: genCSRPEM(t, "dup", key),
	}); err == nil {
		t.Error("重复使用 Join Token 应报错")
	}
}
