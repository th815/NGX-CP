// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package config

import (
	"context"
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore 用 SQLite 内存库构造配置存储（开发态同构，避免引入 PG 依赖）。
func newTestStore(t *testing.T) *ConfigStore {
	t.Helper()
	client, err := repo.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(context.Background()))
	t.Cleanup(func() { _ = client.Close() })
	return New(client)
}

func TestSyncFromAgent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	base := []*agentv1.ConfigFile{
		{Path: "/etc/nginx/nginx.conf", Content: "user nginx;\n"},
		{Path: "/etc/nginx/conf.d/api.conf", Content: "server { listen 80; }\n"},
	}

	// 1) 首次同步 → 创建 file + blob + revision(source=sync)，changed=2。
	changed, err := s.SyncFromAgent(ctx, 1, base)
	require.NoError(t, err)
	assert.Equal(t, 2, changed)

	fls, err := s.ListFiles(ctx, 1)
	require.NoError(t, err)
	require.Len(t, fls, 2)
	var apiID, nginxID int
	for _, f := range fls {
		assert.NotZero(t, f.CurrentRevID)
		assert.NotEmpty(t, f.CurrentSHA)
		assert.Equal(t, "sync", f.Source)
		assert.Equal(t, "agent", f.Author)
		switch f.Path {
		case "/etc/nginx/conf.d/api.conf":
			apiID = f.ID
		case "/etc/nginx/nginx.conf":
			nginxID = f.ID
		}
	}
	require.NotZero(t, apiID)
	_ = nginxID

	revs, err := s.ListRevisions(ctx, apiID, 0)
	require.NoError(t, err)
	require.Len(t, revs, 1)
	assert.Equal(t, 0, revs[0].ParentID, "首版无 parent")

	// 2) 内容未变再同步 → 不产生新版本，changed=0。
	changed, err = s.SyncFromAgent(ctx, 1, base)
	require.NoError(t, err)
	assert.Equal(t, 0, changed)
	revs, err = s.ListRevisions(ctx, apiID, 0)
	require.NoError(t, err)
	assert.Len(t, revs, 1, "内容未变不产新版本")

	// 3) 内容变化 → 产生新版本，parent 指向上一版，changed=1。
	changedFiles := []*agentv1.ConfigFile{
		{Path: "/etc/nginx/nginx.conf", Content: "user nginx;\n"},
		{Path: "/etc/nginx/conf.d/api.conf", Content: "server { listen 8080; }\n"}, // 改了端口
	}
	changed, err = s.SyncFromAgent(ctx, 1, changedFiles)
	require.NoError(t, err)
	assert.Equal(t, 1, changed)
	revs, err = s.ListRevisions(ctx, apiID, 0)
	require.NoError(t, err)
	require.Len(t, revs, 2, "变化后应有 2 个版本")
	// 版本链语义：新版本(revs[0])的 parent 指向上一版(revs[1])。
	assert.Equal(t, revs[1].ID, revs[0].ParentID, "新版本 parent 指向上一版")

	// 4) 两个节点相同内容 → blob 只存一份（去重生效）。
	other := []*agentv1.ConfigFile{
		{Path: "/etc/nginx/nginx.conf", Content: "user nginx;\n"}, // 与 node1 的 nginx.conf 内容相同
	}
	_, err = s.SyncFromAgent(ctx, 2, other)
	require.NoError(t, err)

	// 内容全集：node1 的 nginx.conf、api.conf(v1)、api.conf(v2) + node2 复用 nginx.conf → 共 3 个 blob。
	// 若不去重，node2 会再写一份 nginx.conf 内容 → 4 个。
	total, err := s.client.ConfigBlob.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, total, "相同内容跨节点只存一份 blob")
}
