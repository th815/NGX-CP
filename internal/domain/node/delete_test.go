package node

import (
	"context"
	"testing"
	"time"

	entconfigsnapshot "github.com/th/ngxcp/ent/configsnapshot"
	entdeploytask "github.com/th/ngxcp/ent/deploytask"
	entenrolltoken "github.com/th/ngxcp/ent/enrolltoken"
	entjointoken "github.com/th/ngxcp/ent/jointoken"
	entncf "github.com/th/ngxcp/ent/nodeconfigfile"
	entnode "github.com/th/ngxcp/ent/node"
	entnodecap "github.com/th/ngxcp/ent/nodecapability"
	entnlt "github.com/th/ngxcp/ent/nodelogtarget"
	entrealserver "github.com/th/ngxcp/ent/realserver"
	"github.com/th/ngxcp/internal/repo"
)

// TestDeleteCascade 锁定「删除节点」必须在事务内级联清理 8 张子表，
// 否则 ent 的 RESTRICT 外键会拦截硬删并返回 5000（生产曾因残留 enroll_tokens 报 5000）。
// 覆盖全部子表：enroll_tokens / join_tokens / node_capabilities /
// node_config_files / node_log_targets / config_snapshots / deploy_tasks / real_servers。
func TestDeleteCascade(t *testing.T) {
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:delcascade?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer client.Close()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := New(client, nil)

	n, err := client.Node.Create().
		SetName("rs-del").SetAddress("10.0.0.9").
		SetRole(entnode.RoleRealServer).SetStatus(entnode.StatusOnline).
		Save(ctx)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	id := n.ID

	// 注入 8 张子表记录（最小必填即可，足以触发 FK 约束）。
	if _, err := client.EnrollToken.Create().
		SetTokenHash("h1").SetExpiresAt(time.Now().Add(time.Hour)).SetNodeID(id).Save(ctx); err != nil {
		t.Fatalf("seed enroll_token: %v", err)
	}
	if _, err := client.JoinToken.Create().
		SetTokenHash("h2").SetRole(entjointoken.RoleRealServer).SetExpiresAt(time.Now().Add(time.Hour)).SetNodeID(id).Save(ctx); err != nil {
		t.Fatalf("seed join_token: %v", err)
	}
	if _, err := client.NodeCapability.Create().SetNodeID(id).Save(ctx); err != nil {
		t.Fatalf("seed capability: %v", err)
	}
	if _, err := client.NodeConfigFile.Create().
		SetNodeID(id).SetPath("/etc/nginx/nginx.conf").SetSha256("s").SetSize(1).SetCapturedAt(time.Now()).Save(ctx); err != nil {
		t.Fatalf("seed config_file: %v", err)
	}
	if _, err := client.NodeLogTarget.Create().
		SetNodeID(id).SetPath("/var/log/nginx/access.log").SetType("access").SetCapturedAt(time.Now()).Save(ctx); err != nil {
		t.Fatalf("seed log_target: %v", err)
	}
	if _, err := client.ConfigSnapshot.Create().
		SetNodeID(id).SetPath("/etc/nginx/nginx.conf").SetChecksum("c").SetStoredPath("/v/s.tar.gz").Save(ctx); err != nil {
		t.Fatalf("seed config_snapshot: %v", err)
	}
	if _, err := client.DeployTask.Create().SetNodeID(id).Save(ctx); err != nil {
		t.Fatalf("seed deploy_task: %v", err)
	}
	if _, err := client.RealServer.Create().
		SetNodeID(id).SetVip("10.0.0.10").SetVport(80).SetRip("10.0.0.9").SetRport(80).Save(ctx); err != nil {
		t.Fatalf("seed real_server: %v", err)
	}

	// 执行删除（修复前应被 FK 拦截返回 5000）。
	if err := svc.Delete(ctx, id); err != nil {
		t.Fatalf("Delete 应成功: %v", err)
	}

	// 节点本体已删。
	if exist, _ := client.Node.Query().Where(entnode.ID(id)).Exist(ctx); exist {
		t.Fatalf("节点未被删除")
	}
	// 全部子表记录已级联清空。
	type childCheck struct {
		name string
		n    int
		err  error
	}
	en, e1 := client.EnrollToken.Query().Where(entenrolltoken.HasNodeWith(entnode.ID(id))).Count(ctx)
	jn, e2 := client.JoinToken.Query().Where(entjointoken.HasNodeWith(entnode.ID(id))).Count(ctx)
	cs, e3 := client.ConfigSnapshot.Query().Where(entconfigsnapshot.HasNodeWith(entnode.ID(id))).Count(ctx)
	dt, e4 := client.DeployTask.Query().Where(entdeploytask.HasNodeWith(entnode.ID(id))).Count(ctx)
	rs, e5 := client.RealServer.Query().Where(entrealserver.HasNodeWith(entnode.ID(id))).Count(ctx)
	checks := []childCheck{
		{"enroll_tokens", en, e1},
		{"join_tokens", jn, e2},
		{"config_snapshots", cs, e3},
		{"deploy_tasks", dt, e4},
		{"real_servers", rs, e5},
	}
	for _, c := range checks {
		if c.err != nil {
			t.Fatalf("count %s: %v", c.name, c.err)
		}
		if c.n != 0 {
			t.Fatalf("%s 残留 %d 条子记录", c.name, c.n)
		}
	}
	// capability / config_file / log_target 同样应清空（用 client 直接查）。
	if n, e := client.NodeCapability.Query().Where(entnodecap.HasNodeWith(entnode.ID(id))).Count(ctx); e != nil || n != 0 {
		t.Fatalf("node_capabilities 残留 %d 条 (err=%v)", n, e)
	}
	if n, e := client.NodeConfigFile.Query().Where(entncf.HasNodeWith(entnode.ID(id))).Count(ctx); e != nil || n != 0 {
		t.Fatalf("node_config_files 残留 %d 条 (err=%v)", n, e)
	}
	if n, e := client.NodeLogTarget.Query().Where(entnlt.HasNodeWith(entnode.ID(id))).Count(ctx); e != nil || n != 0 {
		t.Fatalf("node_log_targets 残留 %d 条 (err=%v)", n, e)
	}
}
