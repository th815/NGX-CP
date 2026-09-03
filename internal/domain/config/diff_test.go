// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T022 验收：Diff / DiffNginx 的增删改统计、大文件性能、缩进变化去噪。
package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genNginx 生成 n 行结构化 nginx 配置，seed 用于引入可控差异。
func genNginx(n, seed int) string {
	var b strings.Builder
	b.WriteString("http {\n")
	for i := 0; i < n; i++ {
		b.WriteString("    server {\n")
		b.WriteString(fmt.Sprintf("        listen %d;\n", 8000+((i+seed)%3)))
		b.WriteString(fmt.Sprintf("        server_name s%d.example.com;\n", i))
		b.WriteString("        location / {\n")
		b.WriteString("            proxy_pass http://backend;\n")
		b.WriteString("        }\n")
		b.WriteString("    }\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func TestDiff_AddLine(t *testing.T) {
	old := "server {\n    listen 80;\n}\n"
	// 在 listen 后插入一行
	newC := "server {\n    listen 80;\n    server_name x;\n}\n"
	res := Diff(old, newC)
	assert.Equal(t, 1, res.Stats.Added, "新增一行 → Added=1")
	assert.Equal(t, 0, res.Stats.Deleted)
	require.Len(t, res.Hunks, 1)
}

func TestDiff_DeleteLine(t *testing.T) {
	old := "server {\n    listen 80;\n    server_name x;\n}\n"
	newC := "server {\n    listen 80;\n}\n"
	res := Diff(old, newC)
	assert.Equal(t, 1, res.Stats.Deleted, "删除一行 → Deleted=1")
	assert.Equal(t, 0, res.Stats.Added)
}

func TestDiff_ModifyLine(t *testing.T) {
	old := "server {\n    listen 80;\n}\n"
	newC := "server {\n    listen 8080;\n}\n"
	res := Diff(old, newC)
	// 修改一行 = 1 删 + 1 增（不折叠为 changed）
	assert.Equal(t, 1, res.Stats.Added, "修改一行 → 1 增")
	assert.Equal(t, 1, res.Stats.Deleted, "修改一行 → 1 删")
	assert.Equal(t, 1, res.Stats.Changed, "Changed 记为 1 处变更")
}

func TestDiff_LargeFilePerf(t *testing.T) {
	old := genNginx(5000, 0)
	newC := genNginx(5000, 0)
	// 制造一处差异，确保真正跑过算法而不是短路。
	newC = strings.Replace(newC, "listen 8000;", "listen 8001;", 1)
	start := time.Now()
	res := Diff(old, newC)
	elapsed := time.Since(start)
	// 仅改一行 = 1 删 + 1 增（共 2 处变更行）。
	assert.Equal(t, 1, res.Stats.Added)
	assert.Equal(t, 1, res.Stats.Deleted)
	t.Logf("5000 行 diff 耗时 %v", elapsed)
	require.Less(t, elapsed, 100*time.Millisecond, "大文件 diff 应 < 100ms")
}

func TestDiffNginx_IndentationNoiseSuppressed(t *testing.T) {
	// 两份配置仅缩进不同（2 空格 vs 4 空格），语义相同。
	old := "server {\n  listen 80;\n  location / {\n    proxy_pass http://b;\n  }\n}\n"
	newC := "server {\n    listen 80;\n    location / {\n        proxy_pass http://b;\n    }\n}\n"
	// 纯文本 diff 仍能看到缩进差异（证明去噪是 DiffNginx 的增量行为）。
	raw := Diff(old, newC)
	assert.NotZero(t, raw.Stats.Added+raw.Stats.Deleted, "Diff 不做去噪，应保留缩进差异")
	// DiffNginx 格式化后两份逐字节相同，零差异。
	res := DiffNginx(old, newC)
	assert.Zero(t, res.Stats.Added, "缩进变化经格式化后不产生 add 噪音")
	assert.Zero(t, res.Stats.Deleted, "缩进变化经格式化后不产生 del 噪音")
	assert.Empty(t, res.Hunks, "缩进变化经格式化后无 hunk")
}

func TestDiffNginx_RealChangeStillDetected(t *testing.T) {
	old := "server {\n    listen 80;\n}\n"
	newC := "server {\n    listen 80;\n    server_name y;\n}\n"
	res := DiffNginx(old, newC)
	require.Equal(t, 1, res.Stats.Added, "格式化不掩盖真实新增")
	require.Equal(t, 0, res.Stats.Deleted)
}

func BenchmarkDiff(b *testing.B) {
	old := genNginx(3000, 0) // 约 100KB 量级
	newC := genNginx(3000, 0)
	newC = strings.Replace(newC, "listen 8000;", "listen 8001;", 1)
	b.ResetTimer()
	var total time.Duration
	for i := 0; i < b.N; i++ {
		start := time.Now()
		_ = Diff(old, newC)
		total += time.Since(start)
	}
	b.ReportMetric(float64(total.Nanoseconds())/float64(b.N)/1e6, "ms/op")
}
