// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T068 封禁子系统单测：封禁 → security_block 变更单（含 config_blob/revision）；
// 解封同样是变更单且移除 deny；无节点/非法 IP/事件无 IP 的错误路径；IP 提取。
package security

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/changeorder"
	"github.com/th/ngxcp/ent/configrevision"
	"github.com/th/ngxcp/ent/node"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/repo"
)

// newTestBlockEnv 起内存 sqlite + 自动建表，返回 client 与 BlockService（真实 deploy.Service）。
// 每个测试用唯一库名，避免跨测试节点 name 唯一约束冲突。
func newTestBlockEnv(t *testing.T) (*ent.Client, *BlockService) {
	t.Helper()
	client, err := repo.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(context.Background()))
	return client, NewBlockService(client, deploy.New(client))
}

// seedNode 创建一个 online 的指定角色节点。
func seedNode(t *testing.T, client *ent.Client, name, role, addr string) {
	t.Helper()
	_, err := client.Node.Create().
		SetName(name).
		SetAddress(addr).
		SetRole(node.Role(role)).
		SetStatus(node.StatusOnline).
		Save(context.Background())
	require.NoError(t, err)
}

func TestBlockIP_CreatesSecurityBlockOrder(t *testing.T) {
	client, bs := newTestBlockEnv(t)
	ctx := context.Background()
	seedNode(t, client, "rs-nginx-01", "real_server", "10.0.1.11")

	co, err := bs.BlockIP(ctx, "203.0.113.45", "SQL 注入爆破", "admin")
	require.NoError(t, err)
	require.NotNil(t, co)

	// 变更单类型 / 来源正确
	require.Equal(t, changeorder.TypeSecurityBlock, co.Type)
	require.Equal(t, changeorder.SourceAPI, co.Source)
	// 单节点：不命中「≥2 节点需审批」默认规则 → 直达 pending
	require.Equal(t, changeorder.StatusPending, co.Status)
	// 策略：LVS 优雅灰度 + 自动回滚
	require.Equal(t, "lvs_graceful", co.Strategy.Mode)
	require.True(t, co.Strategy.AutoRollback)
	require.False(t, co.Strategy.ApprovalRequired)
	// 目标节点 / 修订已关联
	require.Len(t, co.TargetNodes, 1)
	require.NotEmpty(t, co.ConfigRevisionIds)

	// 修订指向正确的 blob 内容（含 deny 指令）
	revs, err := client.ConfigRevision.Query().
		Where(configrevision.IDIn(co.ConfigRevisionIds...)).
		WithBlob().
		All(ctx)
	require.NoError(t, err)
	require.Len(t, revs, 1)
	require.Equal(t, "conf.d/zz-block-203_0_113_45.conf", revs[0].Path)
	require.Equal(t, configrevision.SourceSecurityBlock, revs[0].Source)
	require.NotNil(t, revs[0].Edges.Blob)
	require.Contains(t, revs[0].Edges.Blob.Content, "deny 203.0.113.45;")
}

func TestBlockIP_NoNodes(t *testing.T) {
	_, bs := newTestBlockEnv(t)
	// 不建任何节点 → 无可用 Nginx RS 目标 → 应报错
	co, err := bs.BlockIP(context.Background(), "203.0.113.99", "x", "admin")
	require.Error(t, err)
	require.Nil(t, co)
}

func TestBlockIP_InvalidIP(t *testing.T) {
	_, bs := newTestBlockEnv(t)
	_, err := bs.BlockIP(context.Background(), "not-an-ip", "x", "admin")
	require.Error(t, err)
}

func TestUnblockIP_RemovesDeny(t *testing.T) {
	client, bs := newTestBlockEnv(t)
	ctx := context.Background()
	seedNode(t, client, "rs-nginx-01", "real_server", "10.0.1.11")

	_, err := bs.BlockIP(ctx, "198.51.100.7", "扫描", "admin")
	require.NoError(t, err)

	unCo, err := bs.UnblockIP(ctx, "198.51.100.7", "误报", "admin")
	require.NoError(t, err)
	require.NotNil(t, unCo)
	require.Equal(t, changeorder.TypeSecurityBlock, unCo.Type)

	// 解封修订的 blob 内容不应再含 deny 指令
	revs, err := client.ConfigRevision.Query().
		Where(configrevision.IDIn(unCo.ConfigRevisionIds...)).
		WithBlob().
		All(ctx)
	require.NoError(t, err)
	require.Len(t, revs, 1)
	require.NotContains(t, revs[0].Edges.Blob.Content, "deny 198.51.100.7;")
	require.Contains(t, revs[0].Edges.Blob.Content, "# unblocked 198.51.100.7")
}

func TestBlockEvent_NoIP(t *testing.T) {
	_, bs := newTestBlockEnv(t)
	evt := &Event{ID: 1, RuleName: "sql_injection", Sample: "正常流量，无攻击特征"}
	co, err := bs.BlockEvent(context.Background(), evt, "admin")
	require.Error(t, err)
	require.Nil(t, co)
}

func TestBlockEvent_FromSample(t *testing.T) {
	client, bs := newTestBlockEnv(t)
	ctx := context.Background()
	seedNode(t, client, "rs-nginx-01", "real_server", "10.0.1.11")

	evt := &Event{ID: 7, RuleName: "cc_flood", Sample: "client 192.0.2.33 hit 700 req/min from /login"}
	co, err := bs.BlockEvent(ctx, evt, "admin")
	require.NoError(t, err)
	require.NotNil(t, co)
	require.Equal(t, changeorder.TypeSecurityBlock, co.Type)
	require.Len(t, co.ConfigRevisionIds, 1)
}

func TestExtractIP(t *testing.T) {
	cases := []struct {
		sample string
		want   string
		ok     bool
	}{
		{"client 192.0.2.33 hit 700 req/min", "192.0.2.33", true},
		{"10.0.0.1, 10.0.0.2 are scanners", "10.0.0.1", true},
		{"2001:db8::1 attempted access", "2001:db8::1", true},
		{"纯文本无地址", "", false},
	}
	for _, c := range cases {
		got, ok := ExtractIP(c.sample)
		require.Equal(t, c.ok, ok, "sample=%q", c.sample)
		if c.ok {
			require.Equal(t, c.want, got, "sample=%q", c.sample)
		}
	}
}

// TestBlockIP_BlobDedup 验证相同内容只落一份 blob（内容寻址去重）。
func TestBlockIP_BlobDedup(t *testing.T) {
	client, bs := newTestBlockEnv(t)
	ctx := context.Background()
	seedNode(t, client, "rs-nginx-01", "real_server", "10.0.1.11")

	_, err := bs.BlockIP(ctx, "203.0.113.45", "first", "admin")
	require.NoError(t, err)
	// 同一 IP 再封一次（内容相同）→ blob 应复用
	_, err = bs.BlockIP(ctx, "203.0.113.45", "second", "admin")
	require.NoError(t, err)

	blobs, err := client.ConfigBlob.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, blobs, 1, "相同封禁内容应去重为单个 blob")
	// 但应产生两条修订（两次独立变更单）
	revs, err := client.ConfigRevision.Query().
		Where(configrevision.SourceEQ(configrevision.SourceSecurityBlock)).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, revs)
}
