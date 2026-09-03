package capability

import (
	"os"
	"testing"
)

func TestParseNginxV(t *testing.T) {
	raw, err := os.ReadFile("testdata/nginx_V_1.30.0.txt")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	info, err := ParseNginxV(string(raw))
	if err != nil {
		t.Fatalf("ParseNginxV: %v", err)
	}

	if info.Version != "1.30.0" {
		t.Errorf("Version = %q, want 1.30.0", info.Version)
	}
	if info.Prefix != "/etc/nginx" {
		t.Errorf("Prefix = %q, want /etc/nginx", info.Prefix)
	}
	if info.ConfPath != "/etc/nginx/nginx.conf" {
		t.Errorf("ConfPath = %q, want /etc/nginx/nginx.conf", info.ConfPath)
	}
	if info.SbinPath != "/usr/sbin/nginx" {
		t.Errorf("SbinPath = %q, want /usr/sbin/nginx", info.SbinPath)
	}
	if info.BinaryPath != "/usr/sbin/nginx" {
		t.Errorf("BinaryPath = %q, want /usr/sbin/nginx", info.BinaryPath)
	}
	if info.RunUser != "nginx" {
		t.Errorf("RunUser = %q, want nginx", info.RunUser)
	}
	if info.OpenSSLVersion != "3.5.1" {
		t.Errorf("OpenSSLVersion = %q, want 3.5.1", info.OpenSSLVersion)
	}
	if !info.TLSSNI {
		t.Errorf("TLSSNI = false, want true")
	}

	wantMods := []string{
		"http_ssl", "http_v2", "http_v3", "http_realip", "http_stub_status",
		"http_gzip_static", "stream", "stream_ssl", "stream_ssl_preread",
		"nginx_upstream_check_module",
	}
	for _, w := range wantMods {
		if !contains(info.StaticModules, w) {
			t.Errorf("StaticModules missing %q; got %v", w, info.StaticModules)
		}
	}
	if info.ConfigHash == "" {
		t.Errorf("ConfigHash empty")
	}
}

func TestParseNginxVEdge(t *testing.T) {
	// 缺版本行 → 报错
	if _, err := ParseNginxV("built with OpenSSL 3.5.1\nconfigure arguments: --prefix=/x"); err == nil {
		t.Errorf("expected error for missing version line")
	}
	// 空输入 → 报错
	if _, err := ParseNginxV(""); err == nil {
		t.Errorf("expected error for empty input")
	}
	// 模块归一化：--with-stream（无 _module 后缀）应得 "stream"
	info, err := ParseNginxV("nginx version: nginx/1.25.0\nconfigure arguments: --with-stream --add-module=/opt/third_party/nginx_hello_module")
	if err != nil {
		t.Fatalf("ParseNginxV edge: %v", err)
	}
	if !contains(info.StaticModules, "stream") {
		t.Errorf("expected 'stream' from --with-stream, got %v", info.StaticModules)
	}
	if !contains(info.StaticModules, "nginx_hello_module") {
		t.Errorf("expected path-basename module, got %v", info.StaticModules)
	}
	// SNI 缺失 → false
	if info.TLSSNI {
		t.Errorf("expected TLSSNI=false when 'TLS SNI support enabled' absent")
	}
	// 单引号内含空格的参数不被错误切分
	info2, err := ParseNginxV("nginx version: nginx/1.25.0\nconfigure arguments: --with-cc-opt='-g -O2 -Wall' --prefix=/etc/nginx")
	if err != nil {
		t.Fatalf("ParseNginxV quote: %v", err)
	}
	if info2.Prefix != "/etc/nginx" {
		t.Errorf("Prefix = %q (quoted arg split wrongly?)", info2.Prefix)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
