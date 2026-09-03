// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T022 集成测试：ConfigStore.DiffRevisions 基于版本链做语义 diff。
package config

import (
	"context"
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffRevisions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	base := []*agentv1.ConfigFile{
		{Path: "/etc/nginx/conf.d/api.conf", Content: "server {\n    listen 80;\n}\n"},
	}
	_, err := s.SyncFromAgent(ctx, 1, base)
	require.NoError(t, err)

	// 改端口 80 -> 8080，触发新版本。
	changed := []*agentv1.ConfigFile{
		{Path: "/etc/nginx/conf.d/api.conf", Content: "server {\n    listen 8080;\n}\n"},
	}
	_, err = s.SyncFromAgent(ctx, 1, changed)
	require.NoError(t, err)

	fls, err := s.ListFiles(ctx, 1)
	require.NoError(t, err)
	require.Len(t, fls, 1)
	fileID := fls[0].ID

	revs, err := s.ListRevisions(ctx, fileID, 0)
	require.NoError(t, err)
	require.Len(t, revs, 2, "应有两个版本")
	// ListRevisions 倒序：revs[0]=新，revs[1]=旧。
	oldRev, newRev := revs[1].ID, revs[0].ID

	res, err := s.DiffRevisions(ctx, fileID, oldRev, newRev)
	require.NoError(t, err)
	assert.Equal(t, oldRev, res.OldRev)
	assert.Equal(t, newRev, res.NewRev)
	// 改一行 = 1 删 + 1 增。
	assert.Equal(t, 1, res.Stats.Added)
	assert.Equal(t, 1, res.Stats.Deleted)
	require.NotEmpty(t, res.Hunks)
}

func TestDiffRevisions_IndentationNormalized(t *testing.T) {
	// 两版仅缩进不同（2 空格 vs 4 空格），语义相同 → DiffRevisions 应零差异。
	ctx := context.Background()
	s := newTestStore(t)

	base := []*agentv1.ConfigFile{
		{Path: "/etc/nginx/conf.d/api.conf", Content: "server {\n  listen 80;\n}\n"},
	}
	_, err := s.SyncFromAgent(ctx, 1, base)
	require.NoError(t, err)

	reindented := []*agentv1.ConfigFile{
		{Path: "/etc/nginx/conf.d/api.conf", Content: "server {\n    listen 80;\n}\n"},
	}
	_, err = s.SyncFromAgent(ctx, 1, reindented)
	require.NoError(t, err)

	fls, err := s.ListFiles(ctx, 1)
	require.NoError(t, err)
	require.Len(t, fls, 1)
	revs, err := s.ListRevisions(ctx, fls[0].ID, 0)
	require.NoError(t, err)
	require.Len(t, revs, 2)

	res, err := s.DiffRevisions(ctx, fls[0].ID, revs[1].ID, revs[0].ID)
	require.NoError(t, err)
	assert.Zero(t, res.Stats.Added, "缩进变化不进 diff")
	assert.Zero(t, res.Stats.Deleted)
	assert.Empty(t, res.Hunks)
}
