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

// TestRegisterSelfJoin 锁定「web 一键自注册」关键路径（节点绑定令牌模型）：
// 控制台新建节点 → 为该节点签发 Join Token（入库 join_tokens 表、Agent 侧持久化）
// → Agent 持 token + 本地 CSR 注册 → 控制面复用既有节点、签发客户端证书 → 节点上线（无审批）。
// 并验证：同一令牌可对该节点「重建证书」（证书丢失场景），即 1 token = 1 node。
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

	// 1) 控制台新建节点（enrolling），并为该节点签发 Join Token（real_server 角色）。
	n, err := nodeSvc.Create(ctx, node.CreateNodeIn{Name: "nginx-rs-01", Role: "real_server"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	joinTok, _, err := nodeSvc.IssueJoinToken(ctx, n.ID, "real_server", time.Hour)
	if err != nil {
		t.Fatalf("issue join token: %v", err)
	}

	// 2) 持 Join Token + 本地 CSR 自注册（Agent 侧：token 持久化于 /etc/ngxcp/agent.conf）。
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	resp, err := cli.Register(ctx, &agentv1.RegisterRequest{
		JoinToken: joinTok,
		Hostname:  "nginx-rs-01",
		Csr:       genCSRPEM(t, "nginx-rs-01", key),
	})
	if err != nil {
		t.Fatalf("register(join): %v", err)
	}
	if resp.NodeId != int64(n.ID) {
		t.Fatalf("自注册 nodeID = %d, want %d（应为签发令牌时创建的节点）", resp.NodeId, n.ID)
	}
	if len(resp.ClientCert) == 0 || len(resp.CaCert) == 0 {
		t.Fatal("自注册未返回证书")
	}

	// 3) 节点状态应为 online（无审批直接纳管），角色与创建时一致。
	got, err := nodeSvc.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("查询节点: %v", err)
	}
	if got.Status != string(entnode.StatusOnline) {
		t.Errorf("节点状态 = %q, want online（自注册后应直接上线，无审批）", got.Status)
	}
	if got.Role != string(entnode.RoleRealServer) {
		t.Errorf("节点角色 = %q, want real_server", got.Role)
	}

	// 4) 同一令牌可对该节点「重建证书」（证书丢失场景）：应成功且复用同一 nodeID（1 token = 1 node）。
	if _, err := cli.Register(ctx, &agentv1.RegisterRequest{
		JoinToken: joinTok, Hostname: "nginx-rs-01", Csr: genCSRPEM(t, "nginx-rs-01", key),
	}); err != nil {
		t.Errorf("凭同一令牌重建证书应成功（证书丢失场景），但报错: %v", err)
	}
	// 节点数仍应为 1（不会因重建证书而新建节点）。
	cnt, err := client.Node.Query().Count(ctx)
	if err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if cnt != 1 {
		t.Errorf("节点数 = %d, want 1（令牌绑定单一节点，不得扩散）", cnt)
	}
}
