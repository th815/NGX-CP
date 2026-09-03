// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/th/ngxcp/internal/domain/backup"
)

// makeTree 在临时目录造一棵含子目录的假配置树。
func makeTree(t *testing.T) (root, file string) {
	t.Helper()
	root = t.TempDir()
	file = filepath.Join(root, "nginx.conf")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o600))
	require.NoError(t, os.Chmod(file, 0o600))
	sub := filepath.Join(root, "conf.d")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "api.conf"), []byte("server{}"), 0o644))
	return root, file
}

func hasPath(files []backup.SnapshotFile, substr string) bool {
	for _, f := range files {
		if strings.Contains(f.Path, substr) {
			return true
		}
	}
	return false
}

// TestSnapshot_Create_CapturesMetadata：Create 捕获权限/属主/哈希。
func TestSnapshot_Create_CapturesMetadata(t *testing.T) {
	root, _ := makeTree(t)
	staging := t.TempDir()
	ex := NewSnapshotExecutor()

	co, err := ex.Create(context.Background(), SnapshotRequest{
		Paths:      []string{root},
		StagingDir: staging,
		NodeID:     1,
		Type:       "manual",
	})
	require.NoError(t, err)
	require.FileExists(t, co.Path)
	require.NotZero(t, co.Size)

	var mf *backup.SnapshotFile
	for i := range co.Files {
		if strings.HasSuffix(co.Files[i].Path, "nginx.conf") {
			mf = &co.Files[i]
		}
	}
	require.NotNil(t, mf, "nginx.conf 应在快照文件列表中")
	assert.Equal(t, os.FileMode(0o600), os.FileMode(mf.Mode).Perm(), "权限位应被记录")
	assert.NotEmpty(t, mf.Owner, "属主应被记录（name:name 或 uid:gid）")
	assert.NotEmpty(t, mf.SHA256, "SHA256 应被记录")
}

// TestSnapshot_Restore_RestoresContentAndPerms：Restore 还原内容与权限。
func TestSnapshot_Restore_RestoresContentAndPerms(t *testing.T) {
	root, file := makeTree(t)
	ex := NewSnapshotExecutor()
	co, err := ex.Create(context.Background(), SnapshotRequest{
		Paths:      []string{root},
		StagingDir: t.TempDir(),
		Type:       "pre_deploy",
	})
	require.NoError(t, err)

	// 破坏原文件（改内容 + 改权限）
	require.NoError(t, os.WriteFile(file, []byte("TAMPERED"), 0o644))

	require.NoError(t, ex.Restore(context.Background(), RestoreRequest{TarPath: co.Path}))

	got, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got), "内容应被还原")
	fi, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "权限应被还原为 0o600")
}

// TestSnapshot_IncludeSSLToggle：includeSSL=false 排除 ssl 子目录，true 包含。
func TestSnapshot_IncludeSSLToggle(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "ssl"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ssl", "cert.pem"), []byte("CERT"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nginx.conf"), []byte("x"), 0o644))
	ex := NewSnapshotExecutor()

	noSSL, err := ex.Create(context.Background(), SnapshotRequest{
		Paths: []string{root}, StagingDir: t.TempDir(), IncludeSSL: false,
	})
	require.NoError(t, err)
	assert.False(t, hasPath(noSSL.Files, "cert.pem"), "默认应排除 ssl")

	withSSL, err := ex.Create(context.Background(), SnapshotRequest{
		Paths: []string{root}, StagingDir: t.TempDir(), IncludeSSL: true,
	})
	require.NoError(t, err)
	assert.True(t, hasPath(withSSL.Files, "cert.pem"), "includeSSL=true 应包含 ssl")
}
