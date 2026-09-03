// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package config

import (
	"context"
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/internal/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDriftClient 复用与 T025 一致的 sqlite 内存库 + 自动建表。
func newDriftClient(t *testing.T) *ent.Client {
	t.Helper()
	client, err := repo.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(context.Background()))
	t.Cleanup(func() { client.Close() })
	return client
}

// setupDriftNode 创建集群 + 一台 real_server 节点。
func setupDriftNode(t *testing.T, ctx context.Context, client *ent.Client) int {
	t.Helper()
	cl, err := client.Cluster.Create().SetName("c1").Save(ctx)
	require.NoError(t, err)
	n, err := client.Node.Create().
		SetName("rs-01").SetRole("real_server").SetStatus("online").
		SetAddress("10.0.0.11").SetCluster(cl).Save(ctx)
	require.NoError(t, err)
	return n.ID
}

// reported 构造一个带正确 SHA 的实际上报文件。
func reported(path, content string) ReportedConfigFile {
	return ReportedConfigFile{Path: path, SHA: sha256Hex([]byte(content)), Content: content}
}

func syncBaseline(t *testing.T, ctx context.Context, store *ConfigStore, nodeID int, files ...*agentv1.ConfigFile) {
	t.Helper()
	_, err := store.SyncFromAgent(ctx, nodeID, files)
	require.NoError(t, err)
}

func findDrift(items []DriftItem, kind string) *DriftItem {
	for i := range items {
		if items[i].Kind == kind {
			return &items[i]
		}
	}
	return nil
}

// TestDrift_Modified：节点内容被手工改动 → 检出 modified 漂移（nginx.conf 为 critical）。
func TestDrift_Modified(t *testing.T) {
	ctx := context.Background()
	client := newDriftClient(t)
	store := New(client)
	nodeID := setupDriftNode(t, ctx, client)

	syncBaseline(t, ctx, store, nodeID, &agentv1.ConfigFile{
		Path: "/etc/nginx/nginx.conf", Sha256: sha256Hex([]byte("base")), Size: 4, Content: "base",
	})

	d := NewDriftDetector(client, store, DriftConfig{})
	report, err := d.Detect(ctx, nodeID, []ReportedConfigFile{
		reported("/etc/nginx/nginx.conf", "manually edited"),
	})
	require.NoError(t, err)
	require.True(t, report.HasDrift(), "手工改动应检出漂移")
	it := findDrift(report.Items, DriftModified)
	require.NotNil(t, it, "应检出 modified 漂移")
	assert.Equal(t, "critical", it.Severity, "nginx.conf 应 critical")
	require.NotNil(t, it.Diff, "modified 应带 Diff")
}

// TestDrift_NoDrift：节点内容与基线一致 → 无漂移。
func TestDrift_NoDrift(t *testing.T) {
	ctx := context.Background()
	client := newDriftClient(t)
	store := New(client)
	nodeID := setupDriftNode(t, ctx, client)

	syncBaseline(t, ctx, store, nodeID, &agentv1.ConfigFile{
		Path: "/etc/nginx/nginx.conf", Sha256: sha256Hex([]byte("base")), Size: 4, Content: "base",
	})

	d := NewDriftDetector(client, store, DriftConfig{})
	report, err := d.Detect(ctx, nodeID, []ReportedConfigFile{
		reported("/etc/nginx/nginx.conf", "base"),
	})
	require.NoError(t, err)
	assert.False(t, report.HasDrift(), "内容一致不应检出漂移")
}

// TestDrift_Deleted：平台管理的文件在节点上被删除 → 检出 deleted 漂移。
func TestDrift_Deleted(t *testing.T) {
	ctx := context.Background()
	client := newDriftClient(t)
	store := New(client)
	nodeID := setupDriftNode(t, ctx, client)

	syncBaseline(t, ctx, store, nodeID,
		&agentv1.ConfigFile{Path: "/etc/nginx/nginx.conf", Sha256: sha256Hex([]byte("a")), Size: 1, Content: "a"},
		&agentv1.ConfigFile{Path: "/etc/nginx/conf.d/api.conf", Sha256: sha256Hex([]byte("b")), Size: 1, Content: "b"},
	)

	d := NewDriftDetector(client, store, DriftConfig{})
	// 实际上报只剩 nginx.conf，api.conf 被删。
	report, err := d.Detect(ctx, nodeID, []ReportedConfigFile{
		reported("/etc/nginx/nginx.conf", "a"),
	})
	require.NoError(t, err)
	it := findDrift(report.Items, DriftDeleted)
	require.NotNil(t, it, "应检出 deleted 漂移")
	assert.Equal(t, "/etc/nginx/conf.d/api.conf", it.Path)
	assert.Equal(t, "critical", it.Severity, "conf.d/*.conf 应 critical")
}

// TestDrift_Added：节点存在平台未管理的文件 → 检出 added 漂移（未知路径默认 warning）。
func TestDrift_Added(t *testing.T) {
	ctx := context.Background()
	client := newDriftClient(t)
	store := New(client)
	nodeID := setupDriftNode(t, ctx, client)

	syncBaseline(t, ctx, store, nodeID, &agentv1.ConfigFile{
		Path: "/etc/nginx/nginx.conf", Sha256: sha256Hex([]byte("a")), Size: 1, Content: "a",
	})

	d := NewDriftDetector(client, store, DriftConfig{})
	report, err := d.Detect(ctx, nodeID, []ReportedConfigFile{
		reported("/etc/nginx/nginx.conf", "a"),
		reported("/etc/nginx/mime.types", "evil"), // 不在任何规则内 → warning
	})
	require.NoError(t, err)
	it := findDrift(report.Items, DriftAdded)
	require.NotNil(t, it, "应检出 added 漂移")
	assert.Equal(t, "warning", it.Severity, "未匹配规则的路径应 warning")
}

// TestDrift_PlatformManagedOverride：平台主动产生的版本（manual_edit）作为期望，节点未同步时为漂移。
func TestDrift_PlatformManagedOverride(t *testing.T) {
	ctx := context.Background()
	client := newDriftClient(t)
	store := New(client)
	nodeID := setupDriftNode(t, ctx, client)

	// 基线 sync（首次纳管）。
	syncBaseline(t, ctx, store, nodeID, &agentv1.ConfigFile{
		Path: "/etc/nginx/conf.d/api.conf", Sha256: sha256Hex([]byte("v1")), Size: 2, Content: "v1",
	})
	// 平台主动部署 v2（manual_edit）。
	files, err := store.ListFiles(ctx, nodeID)
	require.NoError(t, err)
	require.Len(t, files, 1)
	_, err = store.CreateRevision(ctx, files[0].ID, []byte("v2"), RevisionOpts{
		Source: SourceManualEdit, Author: "admin", Message: "deploy v2",
	})
	require.NoError(t, err)

	d := NewDriftDetector(client, store, DriftConfig{})
	// 节点实际仍是 v1（尚未同步平台部署）。
	report, err := d.Detect(ctx, nodeID, []ReportedConfigFile{
		reported("/etc/nginx/conf.d/api.conf", "v1"),
	})
	require.NoError(t, err)
	it := findDrift(report.Items, DriftModified)
	require.NotNil(t, it, "节点落后平台部署应检出漂移")
	assert.Equal(t, "critical", it.Severity)

	// 节点同步到 v2 后 → 无漂移。
	report2, err := d.Detect(ctx, nodeID, []ReportedConfigFile{
		reported("/etc/nginx/conf.d/api.conf", "v2"),
	})
	require.NoError(t, err)
	assert.False(t, report2.HasDrift(), "节点与平台期望一致应无漂移")
}

// TestDrift_RecordAndGet：RecordActual 写入缓存，GetReport / Reports 可读取。
func TestDrift_RecordAndGet(t *testing.T) {
	ctx := context.Background()
	client := newDriftClient(t)
	store := New(client)
	nodeID := setupDriftNode(t, ctx, client)

	syncBaseline(t, ctx, store, nodeID, &agentv1.ConfigFile{
		Path: "/etc/nginx/nginx.conf", Sha256: sha256Hex([]byte("base")), Size: 4, Content: "base",
	})

	d := NewDriftDetector(client, store, DriftConfig{})
	report, err := d.RecordActual(ctx, nodeID, []ReportedConfigFile{
		reported("/etc/nginx/nginx.conf", "hacked"),
	})
	require.NoError(t, err)
	require.True(t, report.HasDrift())

	got, ok := d.GetReport(nodeID)
	require.True(t, ok, "应能从缓存取到报告")
	require.True(t, got.HasDrift())

	all := d.Reports()
	require.Len(t, all, 1, "Reports 应含该节点")
}

// TestDrift_WorkerScan：scanAll 基于缓存 actual 复检并刷新报告。
func TestDrift_WorkerScan(t *testing.T) {
	ctx := context.Background()
	client := newDriftClient(t)
	store := New(client)
	nodeID := setupDriftNode(t, ctx, client)

	syncBaseline(t, ctx, store, nodeID, &agentv1.ConfigFile{
		Path: "/etc/nginx/nginx.conf", Sha256: sha256Hex([]byte("base")), Size: 4, Content: "base",
	})

	d := NewDriftDetector(client, store, DriftConfig{})
	_, err := d.RecordActual(ctx, nodeID, []ReportedConfigFile{
		reported("/etc/nginx/nginx.conf", "changed"),
	})
	require.NoError(t, err)

	// 直接调用 unexported scanAll（同包可访问）模拟一次定时巡检。
	d.scanAll(ctx)
	got, ok := d.GetReport(nodeID)
	require.True(t, ok)
	require.True(t, got.HasDrift(), "巡检后漂移报告应仍在")
}
