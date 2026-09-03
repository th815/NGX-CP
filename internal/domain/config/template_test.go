// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package config

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRender_ThreeLevelOverride(t *testing.T) {
	ctx := context.Background()
	client := newSemanticClient(t)
	svc := NewTemplateService(client)

	cl, err := client.Cluster.Create().SetName("prod").Save(ctx)
	require.NoError(t, err)
	n1, err := client.Node.Create().SetName("rs-01").SetRole("real_server").
		SetStatus("online").SetAddress("10.0.0.11").SetCluster(cl).Save(ctx)
	require.NoError(t, err)

	// global: cluster=prod, timeout=10s
	require.NoError(t, svc.SetVariable(ctx, ScopeGlobal, 0, "cluster", "prod", false))
	require.NoError(t, svc.SetVariable(ctx, ScopeGlobal, 0, "timeout", "10s", false))
	// cluster: cluster=prod-web（覆盖 global）, weight=5
	require.NoError(t, svc.SetVariable(ctx, ScopeCluster, cl.ID, "cluster", "prod-web", false))
	require.NoError(t, svc.SetVariable(ctx, ScopeCluster, cl.ID, "weight", "5", false))
	// node: timeout=60s（覆盖 cluster）, port=8080
	require.NoError(t, svc.SetVariable(ctx, ScopeNode, n1.ID, "timeout", "60s", false))
	require.NoError(t, svc.SetVariable(ctx, ScopeNode, n1.ID, "port", "8080", false))

	vars, err := svc.ResolveVariables(ctx, n1.ID)
	require.NoError(t, err)

	// 优先级：node > cluster > global
	assert.Equal(t, "prod-web", vars["cluster"], "cluster 应取 cluster 级覆盖")
	assert.Equal(t, "60s", vars["timeout"], "timeout 应取 node 级覆盖")
	assert.Equal(t, "5", vars["weight"], "weight 仅 cluster 级存在")
	assert.Equal(t, "8080", vars["port"], "port 仅 node 级存在")
	_, ok := vars["timeout_unused"]
	assert.False(t, ok, "未定义变量不应出现在合并结果")
}

func TestRender_MissingVariable(t *testing.T) {
	_, err := Render("server {{ .backend_ip }}:{{ .missing }};", map[string]string{
		"backend_ip": "10.0.0.1",
	})
	require.Error(t, err, "缺失变量必须报错")
	assert.Contains(t, err.Error(), "缺少变量", "错误应明确指出缺变量")
	assert.Contains(t, err.Error(), "missing", "错误应点名缺哪个变量")
}

func TestRender_SyntaxError(t *testing.T) {
	_, err := Render("{{ if }}", map[string]string{})
	require.Error(t, err, "模板语法错误必须报错")
	assert.Contains(t, err.Error(), "解析模板失败")
}

func TestRender_HTMLCommentAndRefs(t *testing.T) {
	out, err := Render(
		"# {{ .cluster }}\nupstream {{ .cluster }}_b { server {{ .ip }}; }",
		map[string]string{"cluster": "prod", "ip": "10.0.0.1"},
	)
	require.NoError(t, err)
	assert.True(t, strings.Contains(out, "upstream prod_b"))
	assert.True(t, strings.Contains(out, "server 10.0.0.1"))
}

func TestMaskedValue(t *testing.T) {
	assert.Equal(t, "secret-token", MaskedValue(Variable{Key: "k", Value: "secret-token", Secret: false}))
	assert.Equal(t, "******", MaskedValue(Variable{Key: "k", Value: "secret-token", Secret: true}), "secret 必须打码")
}

func TestTemplateService_RenderForNodes(t *testing.T) {
	ctx := context.Background()
	client := newSemanticClient(t)
	svc := NewTemplateService(client)

	cl, err := client.Cluster.Create().SetName("prod").Save(ctx)
	require.NoError(t, err)
	n1, err := client.Node.Create().SetName("rs-01").SetRole("real_server").
		SetStatus("online").SetAddress("10.0.0.11").SetCluster(cl).Save(ctx)
	require.NoError(t, err)
	n2, err := client.Node.Create().SetName("rs-02").SetRole("real_server").
		SetStatus("online").SetAddress("10.0.0.12").SetCluster(cl).Save(ctx)
	require.NoError(t, err)

	// global: cluster=prod（两节点共有）
	require.NoError(t, svc.SetVariable(ctx, ScopeGlobal, 0, "cluster", "prod", false))
	// cluster: timeout=30s（两节点共有）
	require.NoError(t, svc.SetVariable(ctx, ScopeCluster, cl.ID, "timeout", "30s", false))
	// node1 单独覆盖 timeout=60s
	require.NoError(t, svc.SetVariable(ctx, ScopeNode, n1.ID, "timeout", "60s", false))

	tmplContent := "upstream {{ .cluster }}_backend {\n    check interval={{ .timeout }};\n}\n"
	tmpl, err := svc.CreateTemplate(ctx, "upstream", tmplContent, "conf.d/upstream-{cluster}.conf", []string{"cluster", "timeout"})
	require.NoError(t, err)
	assert.Equal(t, "upstream", tmpl.Name)
	assert.Equal(t, []string{"cluster", "timeout"}, tmpl.Variables)

	out, err := svc.RenderForNodes(ctx, tmpl, []int{n1.ID, n2.ID})
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Contains(t, out[n1.ID], "interval=60s", "node1 应取 node 级 timeout")
	assert.Contains(t, out[n2.ID], "interval=30s", "node2 应取 cluster 级 timeout")
	assert.NotEqual(t, out[n1.ID], out[n2.ID], "两节点因变量不同渲染结果应不同")
}

func TestTemplateService_UpsertVariable(t *testing.T) {
	ctx := context.Background()
	client := newSemanticClient(t)
	svc := NewTemplateService(client)

	require.NoError(t, svc.SetVariable(ctx, ScopeGlobal, 0, "proxy_timeout", "30s", false))
	require.NoError(t, svc.SetVariable(ctx, ScopeGlobal, 0, "proxy_timeout", "60s", false)) // 同键更新

	cl, err := client.Cluster.Create().SetName("prod").Save(ctx)
	require.NoError(t, err)
	n, err := client.Node.Create().SetName("rs-01").SetRole("real_server").
		SetStatus("online").SetAddress("10.0.0.11").SetCluster(cl).Save(ctx)
	require.NoError(t, err)

	vars, err := svc.ResolveVariables(ctx, n.ID) // 真实节点：global 生效
	require.NoError(t, err)
	assert.Equal(t, "60s", vars["proxy_timeout"], "同键重复写入应更新而非重复")
}
