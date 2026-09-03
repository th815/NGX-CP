package config

import (
	"context"
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/internal/domain/config/rules"
	"github.com/th/ngxcp/internal/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSemanticClient(t *testing.T) *ent.Client {
	t.Helper()
	client, err := repo.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(context.Background()))
	t.Cleanup(func() { client.Close() })
	return client
}

func TestSemanticCheck_Integration(t *testing.T) {
	ctx := context.Background()
	client := newSemanticClient(t)
	store := New(client)

	// 集群 + 两台 RS。
	cl, err := client.Cluster.Create().SetName("c1").Save(ctx)
	require.NoError(t, err)
	rs1, err := client.Node.Create().SetName("rs-01").SetRole("real_server").SetStatus("online").SetAddress("10.0.0.11").SetCluster(cl).Save(ctx)
	require.NoError(t, err)
	rs2, err := client.Node.Create().SetName("rs-02").SetRole("real_server").SetStatus("online").SetAddress("10.0.0.12").SetCluster(cl).Save(ctx)
	require.NoError(t, err)

	// rs1 缺 check 模块；rs2 有 check 模块（双机不一致）。
	_, err = client.NodeCapability.Create().SetNode(rs1).SetModules([]string{"http_ssl"}).SetVersion("1.30.0").Save(ctx)
	require.NoError(t, err)
	_, err = client.NodeCapability.Create().SetNode(rs2).SetModules([]string{"http_ssl", "nginx_upstream_check_module"}).SetVersion("1.30.0").Save(ctx)
	require.NoError(t, err)

	// rs1 的配置用到 check 指令。
	_, err = store.SyncFromAgent(ctx, rs1.ID, []*agentv1.ConfigFile{{
		Path:    "/etc/nginx/nginx.conf",
		Sha256: "abc",
		Size:    80,
		Content: "upstream u { server 1.1.1.1; check interval=3000; }",
	}})
	require.NoError(t, err)

	checker := NewSemanticChecker(client, store, rules.DefaultConfig())
	issues, err := checker.Check(ctx, rs1.ID)
	require.NoError(t, err)

	// 应检出：error（rs1 缺 check 模块）+ warning（rs2 有而 rs1 无，双机漂移）。
	require.NotNil(t, findIssue(issues, "module_check", "error"), "应报模块缺失 error")
	require.NotNil(t, findIssue(issues, "module_check", "warning"), "应报双机不一致 warning")
}

func TestSemanticCheck_CleanConfig(t *testing.T) {
	ctx := context.Background()
	client := newSemanticClient(t)
	store := New(client)

	cl, err := client.Cluster.Create().SetName("c1").Save(ctx)
	require.NoError(t, err)
	rs1, err := client.Node.Create().SetName("rs-01").SetRole("real_server").SetStatus("online").SetAddress("10.0.0.11").SetCluster(cl).Save(ctx)
	require.NoError(t, err)
	_, err = client.NodeCapability.Create().SetNode(rs1).SetModules([]string{"http_ssl", "nginx_upstream_check_module"}).SetVersion("1.30.0").Save(ctx)
	require.NoError(t, err)

	_, err = store.SyncFromAgent(ctx, rs1.ID, []*agentv1.ConfigFile{{
		Path:    "/etc/nginx/nginx.conf",
		Sha256: "xyz",
		Size:    40,
		Content: "upstream u { server 1.1.1.1; }",
	}})
	require.NoError(t, err)

	checker := NewSemanticChecker(client, store, rules.DefaultConfig())
	issues, err := checker.Check(ctx, rs1.ID)
	require.NoError(t, err)
	assert.Nil(t, findIssue(issues, "module_check", "error"), "模块齐全不应报 error")
}

func findIssue(issues []rules.Issue, ruleID, sev string) *rules.Issue {
	for i := range issues {
		if issues[i].RuleID == ruleID && issues[i].Severity == sev {
			return &issues[i]
		}
	}
	return nil
}
