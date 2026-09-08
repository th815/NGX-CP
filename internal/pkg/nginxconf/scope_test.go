// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package nginxconf

import "testing"

// find 返回首个匹配指令名的条目。
func find(ds []ScopedDirective, name string) (ScopedDirective, bool) {
	for _, d := range ds {
		if d.Name == name {
			return d, true
		}
	}
	return ScopedDirective{}, false
}

func TestScanScoped_BlockPath(t *testing.T) {
	conf := `
user nginx;
http {
    include /etc/nginx/conf.d/*.conf;
    server {
        server_name a.example.com;
        location /api {
            access_log /var/log/nginx/api.log main;
        }
    }
}
`
	ds := ScanScoped(conf)

	user, ok := find(ds, "user")
	if !ok || user.Depth() != 0 {
		t.Fatalf("user 应为顶层指令，got %+v", user)
	}
	inc, ok := find(ds, "include")
	if !ok {
		t.Fatal("未扫到 include")
	}
	if !inc.InScope("http") || inc.Depth() != 1 {
		t.Fatalf("include 应在 http 块内且深度 1，got scope=%v", inc.Scope)
	}
	al, ok := find(ds, "access_log")
	if !ok {
		t.Fatal("未扫到 access_log")
	}
	if !al.InScope("http") || !al.InScope("server") || !al.InScope("location") {
		t.Fatalf("access_log 作用域应含 http/server/location，got %v", al.Scope)
	}
	if al.Depth() != 3 {
		t.Fatalf("access_log 深度应为 3，got %d：%v", al.Depth(), al.Scope)
	}
	// 块头带参数时应保留完整块头，便于 UI 展示"哪个 location 覆盖了日志"。
	if al.Scope[2] != "location /api" {
		t.Fatalf("location 块头应保留参数，got %q", al.Scope[2])
	}
}

func TestScanScoped_TopLevelIncludeNotInHTTP(t *testing.T) {
	// 真实踩坑形态：include 放在 main 层级，此时下发 log_format 片段会被 nginx -t 拒绝。
	conf := "include /etc/nginx/conf.d/*.conf;\nhttp {\n  sendfile on;\n}\n"
	ds := ScanScoped(conf)
	inc, ok := find(ds, "include")
	if !ok {
		t.Fatal("未扫到 include")
	}
	if inc.InScope("http") {
		t.Fatalf("main 层级 include 不应被判为 http 内，scope=%v", inc.Scope)
	}
}

func TestScanScoped_CommentsQuotesAndMissingSemicolon(t *testing.T) {
	conf := `http {
    # access_log /commented.log main;
    server {
        access_log "/var/log/ng x/a.log" json   # 行尾注释，且缺分号
    }
}`
	ds := ScanScoped(conf)
	for _, d := range ds {
		if d.Name == "access_log" && d.Args[0] == "/commented.log" {
			t.Fatal("注释内的指令不应被扫出")
		}
	}
	al, ok := find(ds, "access_log")
	if !ok {
		t.Fatal("缺分号的块内末条指令应被收尾产出")
	}
	if al.Args[0] != "/var/log/ng x/a.log" || al.Args[1] != "json" {
		t.Fatalf("引号路径解析错：%v", al.Args)
	}
}

func TestScanScoped_UnbalancedBracesNoPanic(t *testing.T) {
	// 多余 `}` 与未闭合块都不应 panic（配置可能被手工改坏）。
	for _, conf := range []string{"}", "http {", "http { server { listen 80;", "}}}http{listen 80;}"} {
		_ = ScanScoped(conf)
	}
}
