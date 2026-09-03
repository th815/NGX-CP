// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package nginxconf

import (
	"reflect"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"简单", `a b c`, []string{"a", "b", "c"}},
		{"多余空白", "  a \t  b  \n c ", []string{"a", "b", "c"}},
		{"双引号含空格", `access_log "/var/log/my access.log" main;`, []string{"access_log", "/var/log/my access.log", "main"}},
		{"单引号含空格", `error_log '/var/log/my error.log' warn;`, []string{"error_log", "/var/log/my error.log", "warn"}},
		{"引号内的井号不截断", `access_log "/var/log/a#b.log" main;`, []string{"access_log", "/var/log/a#b.log", "main"}},
		{"行尾注释", `access_log /var/log/a.log main; # 注释`, []string{"access_log", "/var/log/a.log", "main"}},
		{"引号内含分号", `access_log "/a;b.log" main;`, []string{"access_log", "/a;b.log", "main"}},
		{"空串参数", `x "" y`, []string{"x", "", "y"}},
		{"空输入", "", nil},
		{"纯注释", `# 只有注释`, nil},
		{"结尾分号被去", `a b;`, []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SplitArgs(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("SplitArgs(%q) = %#v, 期望 %#v", c.in, got, c.want)
			}
		})
	}
}

func TestStripComment(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a b # 注释`, `a b `},
		{`a "/x#y" # 真注释`, `a "/x#y" `},
		{`无注释`, `无注释`},
		{`# 整行注释`, ``},
		{`a '#y' tail`, `a '#y' tail`},
	}
	for _, c := range cases {
		if got := StripComment(c.in); got != c.want {
			t.Errorf("StripComment(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestSplitDirective(t *testing.T) {
	name, args, ok := SplitDirective(`access_log /var/log/a.log main buffer=32k;`)
	if !ok || name != "access_log" || !reflect.DeepEqual(args, []string{"/var/log/a.log", "main", "buffer=32k"}) {
		t.Fatalf("SplitDirective = %q, %#v, %v", name, args, ok)
	}
	if _, _, ok := SplitDirective(`  `); ok {
		t.Error("空行应返回 ok=false")
	}
	if _, _, ok := SplitDirective(`# 注释`); ok {
		t.Error("纯注释应返回 ok=false")
	}
}

// TestScanDirectives 重点锁定块结构处理：块头（http/server/location）必须与块内
// 首条指令切分开，否则块内指令会被粘进块头的参数里而全部丢失（曾踩过的坑）。
func TestScanDirectives(t *testing.T) {
	cfg := `user nginx;   # 注释
http {
    access_log /var/log/nginx/access.log main;
    server {
        listen 80;
        access_log
            /var/log/nginx/vhost.log
            json;
        location /api {
            access_log off;
        }
    }
}
`
	got := ScanDirectives(cfg)
	want := [][]string{
		{"user", "nginx"},
		{"http"},
		{"access_log", "/var/log/nginx/access.log", "main"},
		{"server"},
		{"listen", "80"},
		{"access_log", "/var/log/nginx/vhost.log", "json"}, // 跨行指令
		{"location", "/api"},
		{"access_log", "off"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanDirectives:\n got = %#v\nwant = %#v", got, want)
	}
}

// TestScanDirectivesEdge 边界：空输入、无分号结尾、嵌套大括号、引号内含分隔符。
func TestScanDirectivesEdge(t *testing.T) {
	if got := ScanDirectives(""); len(got) != 0 {
		t.Errorf("空输入应返回空，实际 %#v", got)
	}
	// 末尾漏写分号也要产出（容忍不严谨的手写配置）。
	got := ScanDirectives(`error_log /var/log/nginx/error.log warn`)
	if len(got) != 1 || got[0][0] != "error_log" || len(got[0]) != 3 {
		t.Errorf("漏写分号处理错误: %#v", got)
	}
	// 引号内的 ; { } # 均为字面量。
	got = ScanDirectives(`access_log "/a;b{c}#d.log" main;`)
	if len(got) != 1 || len(got[0]) != 3 || got[0][1] != "/a;b{c}#d.log" {
		t.Errorf("引号内特殊字符处理错误: %#v", got)
	}
}

func TestIsDirectiveComplete(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`access_log /a.log main;`, true},
		{`access_log /a.log`, false},
		{`access_log "/a;b.log"`, false}, // 引号内的分号不结束指令
		{`access_log "/a;b.log";`, true},
		{``, false},
	}
	for _, c := range cases {
		if got := IsDirectiveComplete(c.in); got != c.want {
			t.Errorf("IsDirectiveComplete(%q) = %v, 期望 %v", c.in, got, c.want)
		}
	}
}
