// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T021 集成测试：SaveConfigTree 在注入 ConfigStore 后，应把带内容的配置树同步进
// 内容寻址版本化存储（config_file / config_revision / config_blob）。
package node_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/repo"
	"github.com/stretchr/testify/require"
)

// TestSaveConfigTreeSyncsVersionedStore 验证节点服务的配置树保存会驱动 T021 版本化存储。
func TestSaveConfigTreeSyncsVersionedStore(t *testing.T) {
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:ngxcp_node_cfg?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, client.Schema.Create(ctx))

	n, err := client.Node.Create().
		SetName("rs-01").SetAddress("10.0.0.1").SetRole("real_server").SetStatus("enrolling").
		Save(ctx)
	require.NoError(t, err)

	store := config.New(client)
	svc := node.New(client, store)

	content := "user nginx;\n"
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	files := []*agentv1.ConfigFile{
		{Path: "/etc/nginx/nginx.conf", Content: content, Sha256: sha, Size: int64(len(content))},
		{Path: "/etc/nginx/conf.d/api.conf", Content: "server { listen 80; }\n", Size: 22},
	}

	require.NoError(t, svc.SaveConfigTree(ctx, n.ID, files))

	// 版本化存储应记录这两个文件，且当前版本内容正确。
	list, err := store.ListFiles(ctx, n.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	byPath := make(map[string]*config.FileView, len(list))
	for _, f := range list {
		require.NotZero(t, f.CurrentRevID, "应有当前版本")
		require.NotEmpty(t, f.CurrentSHA, "应有当前版本哈希")
		byPath[f.Path] = f
	}
	nginx, ok := byPath["/etc/nginx/nginx.conf"]
	require.True(t, ok, "应包含 nginx.conf")
	got, err := store.GetCurrentContent(ctx, nginx.ID)
	require.NoError(t, err)
	require.Equal(t, content, string(got))
}
