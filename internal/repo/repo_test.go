package repo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/ent"
	configfile "github.com/th/ngxcp/ent/configfile"
)

// openTestClient 按环境变量 NGXCP_DB_DRIVER / NGXCP_DB_DSN 选择数据库：
//   - 未设置 → 默认 SQLite（临时文件，自动 WAL）
//   - postgres 但连接失败 → Skip（无 docker PG 时 `make test` 仍全绿）
//
// 这是 T006「双路径验证」的核心：同一份测试代码在 SQLite 与 PostgreSQL 上都须通过。
func openTestClient(t *testing.T) *ent.Client {
	t.Helper()
	driver := os.Getenv("NGXCP_DB_DRIVER")
	if driver == "" {
		driver = "sqlite"
	}
	var dsn string
	if v := os.Getenv("NGXCP_DB_DSN"); v != "" {
		dsn = v
	} else {
		// 每个测试用独立临时库，互不污染
		dsn = fmt.Sprintf("file:%s?cache=shared&_fk=1",
			filepath.Join(t.TempDir(), "repo_test.db"))
	}

	client, err := Open(driver, dsn)
	if err != nil {
		if driver == "postgres" {
			t.Skipf("postgres 不可用，跳过（需 docker PG）：%v", err)
		}
		t.Fatalf("打开 %s 客户端失败: %v", driver, err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	// 测试前清空 schema，保证可重复运行
	if err := client.Schema.Create(ctx); err != nil {
		if driver == "postgres" {
			t.Skipf("postgres schema 创建失败，跳过：%v", err)
		}
		t.Fatalf("创建 schema 失败: %v", err)
	}
	return client
}

func TestDualPathSchemaAndCRUD(t *testing.T) {
	client := openTestClient(t)
	ctx := context.Background()

	// 1) 集群 + 节点（验证枚举、边、时间戳）
	cluster := client.Cluster.Create().
		SetName("prod-web").
		SetDescription("生产 Web 集群").
		SaveX(ctx)

	node, err := client.Node.Create().
		SetName("rs-nginx-01").
		SetAddress("10.0.1.11").
		SetRole("real_server").
		SetStatus("enrolling").
		SetLvsWeight(3).
		SetCluster(cluster).
		Save(ctx)
	require.NoError(t, err)
	require.Equal(t, "real_server", node.Role.String())
	require.Equal(t, 3, node.LvsWeight)
	require.False(t, node.CreatedAt.IsZero())

	// 2) 内容寻址 blob + 版本链 + 当前指针
	blob := client.ConfigBlob.Create().
		SetSha256("abc123").
		SetSize(10).
		SetContent("worker_processes 1;").
		SaveX(ctx)

	parentRev := client.ConfigRevision.Create().
		SetNodeID(node.ID).
		SetPath("/etc/nginx/nginx.conf").
		SetBlob(blob).
		SetMessage("基线").
		SaveX(ctx)

	childRev := client.ConfigRevision.Create().
		SetNodeID(node.ID).
		SetPath("/etc/nginx/nginx.conf").
		SetBlob(blob).
		SetParent(parentRev).
		SaveX(ctx)

	cf := client.ConfigFile.Create().
		SetNodeID(node.ID).
		SetPath("/etc/nginx/nginx.conf").
		SetFormat("nginx").
		SetCurrentRevision(childRev).
		SaveX(ctx)

	// 回查：从 ConfigFile 反查当前版本 → 父版本链
	got := client.ConfigFile.Query().
		Where(configfile.Path(cf.Path)).
		WithCurrentRevision(func(q *ent.ConfigRevisionQuery) {
			q.WithParent()
		}).
		OnlyX(ctx)
	require.Equal(t, childRev.ID, got.Edges.CurrentRevision.ID)
	require.NotNil(t, got.Edges.CurrentRevision.Edges.Parent)
	require.Equal(t, parentRev.ID, got.Edges.CurrentRevision.Edges.Parent.ID)

	// 3) 变更单 + 发布任务（验证状态机枚举与双向边）
	co := client.ChangeOrder.Create().
		SetTitle("升级 upstream 配置").
		SetStatus("applying").
		SetPriority("high").
		SaveX(ctx)

	dt := client.DeployTask.Create().
		SetState("running").
		SetPhase("validate").
		SetNode(node).
		SetChangeOrder(co).
		SaveX(ctx)

	// 通过 ChangeOrder 反向查询 DeployTask
	tasks := co.QueryDeployTasks().AllX(ctx)
	require.Len(t, tasks, 1)
	require.Equal(t, dt.ID, tasks[0].ID)

	// 通过 Node 反向查询 DeployTask
	nodeTasks := node.QueryDeployTasks().AllX(ctx)
	require.Len(t, nodeTasks, 1)

	// 4) 审计日志（枚举 + 不可变时间戳）
	al := client.AuditLog.Create().
		SetActor("system").
		SetAction("config_deploy").
		SetTargetType("node").
		SetTargetID(node.ID).
		SaveX(ctx)
	require.Equal(t, "config_deploy", al.Action.String())
	require.False(t, al.CreatedAt.IsZero())
}
