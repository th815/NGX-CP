// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package capability

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestExtractLogTargets 覆盖 nginx 日志指令的全部常见形态（含 T018 任务书列出的 7 种）。
func TestExtractLogTargets(t *testing.T) {
	dump := `# configuration file /etc/nginx/nginx.conf:
user  nginx;
worker_processes  auto;

error_log  /var/log/nginx/error.log warn;   # 行尾注释不得污染解析
error_log  /var/log/nginx/api.error.log error;
error_log  stderr;

http {
    access_log /var/log/nginx/access.log main;
    access_log /var/log/nginx/access.log main buffer=32k flush=5s;
    access_log /var/log/nginx/api.access.log json;
    access_log off;
    access_log syslog:server=10.0.1.5:514,facility=local7;
    access_log "/var/log/nginx/my access.log" main;
    access_log /var/log/nginx/$host.access.log main;
    access_log
        /var/log/nginx/multiline.log
        main;

    server {
        access_log /var/log/nginx/vhost.access.log gzip=1 flush=5s;
        error_log  /var/log/nginx/vhost.error.log;
        error_log  syslog:server=10.0.1.5:514;
        error_log  memory:32m debug;
    }
}
`
	got := ExtractLogTargets([]ConfigFile{{Path: "/etc/nginx/nginx.conf", Content: dump}})

	byKey := make(map[string]LogTarget, len(got))
	for _, g := range got {
		byKey[g.Type+"\x00"+g.Path] = g
	}

	// 期望保留的可采集目标（按 type\0path 索引）
	want := []struct {
		typ, path, format, level string
		hasVar                   bool
	}{
		{LogTypeAccess, "/var/log/nginx/access.log", "main", "", false},
		{LogTypeAccess, "/var/log/nginx/api.access.log", "json", "", false},
		{LogTypeAccess, "/var/log/nginx/my access.log", "main", "", false},
		{LogTypeAccess, "/var/log/nginx/$host.access.log", "main", "", true},
		{LogTypeAccess, "/var/log/nginx/multiline.log", "main", "", false},
		{LogTypeAccess, "/var/log/nginx/vhost.access.log", "", "", false}, // gzip=1 是选项不是格式名
		{LogTypeError, "/var/log/nginx/error.log", "", "warn", false},
		{LogTypeError, "/var/log/nginx/api.error.log", "", "error", false},
		{LogTypeError, "/var/log/nginx/vhost.error.log", "", "", false},
	}
	for _, w := range want {
		key := w.typ + "\x00" + w.path
		g, ok := byKey[key]
		if !ok {
			t.Fatalf("缺少目标 %q（实际 %d 条：%+v）", key, len(got), got)
		}
		if g.Format != w.format {
			t.Errorf("%s: Format = %q, 期望 %q", w.path, g.Format, w.format)
		}
		if g.Level != w.level {
			t.Errorf("%s: Level = %q, 期望 %q", w.path, g.Level, w.level)
		}
		if g.HasVariable != w.hasVar {
			t.Errorf("%s: HasVariable = %v, 期望 %v", w.path, g.HasVariable, w.hasVar)
		}
		if g.IsOff || g.IsSyslog {
			t.Errorf("%s: 不应标记为 off/syslog", w.path)
		}
	}

	// 同路径去重：access.log 出现两次（plain + buffer/flush），应只保留一条。
	if n := countPath(got, "/var/log/nginx/access.log"); n != 1 {
		t.Errorf("access.log 去重失败，出现 %d 次", n)
	}

	// off / syslog / stderr / memory 不应产生带路径的采集目标，且必须保留各自的 SkipReason
	// （若去重键不含 SkipReason，error_log 的三种非文件目标会被误合并成一条）。
	for _, g := range got {
		if g.Path == "off" {
			t.Errorf("不应把 off 当文件名: %+v", g)
		}
		if g.Path == "" && g.SkipReason == "" {
			t.Errorf("不采集的条目必须给出 SkipReason: %+v", g)
		}
	}
	wantSkip := map[string]bool{
		"access\x00\x00off":    true,
		"access\x00\x00syslog": true,
		"error\x00\x00stderr":  true,
		"error\x00\x00syslog":  true,
		"error\x00\x00memory":  true,
	}
	gotSkip := map[string]bool{}
	for _, g := range got {
		if g.Path == "" {
			gotSkip[g.Type+"\x00"+g.Path+"\x00"+g.SkipReason] = true
		}
	}
	if !reflect.DeepEqual(gotSkip, wantSkip) {
		t.Errorf("非采集条目 = %v, 期望 %v", gotSkip, wantSkip)
	}

	// 输出顺序稳定：按 type 再按 path。
	for i := 1; i < len(got); i++ {
		if got[i-1].Type > got[i].Type ||
			(got[i-1].Type == got[i].Type && got[i-1].Path > got[i].Path) {
			t.Fatalf("输出未排序: %+v 在 %+v 之前", got[i-1], got[i])
		}
	}
}

// TestExtractLogTargetsEmpty 空配置树不产生任何目标，且不得 panic。
func TestExtractLogTargetsEmpty(t *testing.T) {
	if got := ExtractLogTargets(nil); len(got) != 0 {
		t.Fatalf("空输入应返回空，实际 %+v", got)
	}
	if got := ExtractLogTargets([]ConfigFile{{Path: "a.conf", Content: "# 只有注释\n\n"}}); len(got) != 0 {
		t.Fatalf("纯注释应返回空，实际 %+v", got)
	}
}

// TestStatLogTargets 验证 stat 补全 Size/Inode，且跳过 off/syslog 目标。
func TestStatLogTargets(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "access.log")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	targets := []LogTarget{
		{Path: p, Type: LogTypeAccess},
		{Path: filepath.Join(dir, "nope.log"), Type: LogTypeError},
		{Path: "", Type: LogTypeAccess, IsOff: true},
		{Path: "", Type: LogTypeAccess, IsSyslog: true},
	}
	got := StatLogTargets(targets)

	if got[0].Size != 5 {
		t.Errorf("Size = %d, 期望 5", got[0].Size)
	}
	if got[0].Inode == 0 {
		t.Error("Inode 应被填充（logrotate 轮转检测依赖它）")
	}
	if got[1].StatErr == "" {
		t.Error("不存在的文件应记录 StatErr")
	}
	if got[2].Size != 0 || got[3].Size != 0 {
		t.Error("off/syslog 目标不应执行 stat")
	}
}

func countPath(ts []LogTarget, path string) int {
	n := 0
	for _, t := range ts {
		if t.Path == path {
			n++
		}
	}
	return n
}
