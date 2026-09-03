// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 tianhao

package health_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent/health"
)

// fakeExecutor 实现 health.CommandExecutor，按命令/路径返回预置结果，便于纯逻辑测试。
type fakeExecutor struct {
	outputs  map[string]string // key: "name arg1 arg2 ..."
	cmdErr   map[string]bool   // 该命令返回错误（模拟命令不存在/失败）
	exists   map[string]bool
	stats    map[string]health.FileInfo
	writable map[string]bool
	dirs     map[string][]string
	files    map[string]string // path -> content
}

func newFake() *fakeExecutor {
	return &fakeExecutor{
		outputs:  map[string]string{},
		cmdErr:   map[string]bool{},
		exists:   map[string]bool{},
		stats:    map[string]health.FileInfo{},
		writable: map[string]bool{},
		dirs:     map[string][]string{},
		files:    map[string]string{},
	}
}

func cmdKey(name string, args ...string) string {
	return name + " " + strings.Join(args, " ")
}

func (f *fakeExecutor) Output(_ context.Context, name string, args ...string) (string, error) {
	key := cmdKey(name, args...)
	if f.cmdErr[key] {
		return "", fmt.Errorf("command failed: %s", key)
	}
	if v, ok := f.outputs[key]; ok {
		return v, nil
	}
	return "", fmt.Errorf("unexpected command: %s", key)
}

func (f *fakeExecutor) Stat(path string) (health.FileInfo, error) {
	if fi, ok := f.stats[path]; ok {
		return fi, nil
	}
	if f.exists[path] {
		return health.FileInfo{Exists: true}, nil
	}
	return health.FileInfo{Exists: false}, nil
}

func (f *fakeExecutor) Exists(path string) bool                       { return f.exists[path] }
func (f *fakeExecutor) IsWritableDir(path string) bool                { return f.writable[path] }
func (f *fakeExecutor) ReadDir(dir string) ([]string, error) {
	if d, ok := f.dirs[dir]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("dir not found: %s", dir)
}
func (f *fakeExecutor) ReadFile(path string) (string, error) {
	if c, ok := f.files[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("file not found: %s", path)
}

// findItem 在报告里按 name 找项并返回其 passed。
func findItem(t *testing.T, name string, items []*agentv1.ComplianceItem) bool {
	t.Helper()
	for _, it := range items {
		if it.GetName() == name {
			return it.GetPassed()
		}
	}
	t.Fatalf("item %s not found", name)
	return false
}

func TestRunCompliance(t *testing.T) {
	ctx := context.Background()

	t.Run("vip_on_lo skip when no vips", func(t *testing.T) {
		f := newFake()
		rep, err := health.RunCompliance(ctx, f, health.ComplianceOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if !findItem(t, "vip_on_lo", rep.GetItems()) {
			t.Error("vip_on_lo 应跳过且不阻断")
		}
	})

	t.Run("vip_on_lo detect missing", func(t *testing.T) {
		f := newFake()
		f.outputs[cmdKey("ip", "addr", "show", "lo")] = "inet 127.0.0.1/8 scope host lo"
		rep, _ := health.RunCompliance(ctx, f, health.ComplianceOpts{VIPs: []string{"10.0.0.10/32"}})
		if findItem(t, "vip_on_lo", rep.GetItems()) {
			t.Error("vip_on_lo 缺失应判定不通过")
		}
	})

	t.Run("vip_on_lo pass when bound", func(t *testing.T) {
		f := newFake()
		f.outputs[cmdKey("ip", "addr", "show", "lo")] = "inet 10.0.0.10/32 scope host lo"
		rep, _ := health.RunCompliance(ctx, f, health.ComplianceOpts{VIPs: []string{"10.0.0.10/32"}})
		if !findItem(t, "vip_on_lo", rep.GetItems()) {
			t.Error("vip_on_lo 已绑定应通过")
		}
	})

	t.Run("arp_suppress", func(t *testing.T) {
		f := newFake()
		f.outputs[cmdKey("sysctl", "-n", "net.ipv4.conf.all.arp_ignore")] = "1"
		f.outputs[cmdKey("sysctl", "-n", "net.ipv4.conf.all.arp_announce")] = "2"
		rep, _ := health.RunCompliance(ctx, f, health.ComplianceOpts{})
		if !findItem(t, "arp_suppress", rep.GetItems()) {
			t.Error("arp_ignore=1 arp_announce=2 应通过")
		}

		f2 := newFake()
		f2.outputs[cmdKey("sysctl", "-n", "net.ipv4.conf.all.arp_ignore")] = "0"
		f2.outputs[cmdKey("sysctl", "-n", "net.ipv4.conf.all.arp_announce")] = "0"
		rep2, _ := health.RunCompliance(ctx, f2, health.ComplianceOpts{})
		if findItem(t, "arp_suppress", rep2.GetItems()) {
			t.Error("arp 未抑制应判定不通过")
		}
	})

	t.Run("keepalived unicast detection", func(t *testing.T) {
		f := newFake()
		rep, _ := health.RunCompliance(ctx, f, health.ComplianceOpts{})
		if !findItem(t, "keepalived_unicast", rep.GetItems()) {
			t.Error("无 keepalived.conf 应跳过不阻断")
		}

		f2 := newFake()
		f2.exists["/etc/keepalived/keepalived.conf"] = true
		f2.files["/etc/keepalived/keepalived.conf"] = "unicast_src_ip 10.0.0.2\nunicast_peer {\n 10.0.0.3\n}\n"
		rep2, _ := health.RunCompliance(ctx, f2, health.ComplianceOpts{})
		if !findItem(t, "keepalived_unicast", rep2.GetItems()) {
			t.Error("unicast 配置应通过")
		}

		f3 := newFake()
		f3.exists["/etc/keepalived/keepalived.conf"] = true
		f3.files["/etc/keepalived/keepalived.conf"] = "vrrp_multicast_group4 224.0.0.18\n"
		rep3, _ := health.RunCompliance(ctx, f3, health.ComplianceOpts{})
		if findItem(t, "keepalived_unicast", rep3.GetItems()) {
			t.Error("multicast 配置应判定不通过")
		}
	})

	t.Run("no_ah_auth", func(t *testing.T) {
		f := newFake()
		f.exists["/etc/keepalived/keepalived.conf"] = true
		f.files["/etc/keepalived/keepalived.conf"] = "auth_type AH\n"
		rep, _ := health.RunCompliance(ctx, f, health.ComplianceOpts{})
		if findItem(t, "no_ah_auth", rep.GetItems()) {
			t.Error("引用 AH 认证应判定不通过")
		}

		f2 := newFake()
		f2.exists["/etc/keepalived/keepalived.conf"] = true
		f2.files["/etc/keepalived/keepalived.conf"] = "auth_type PASS\n"
		rep2, _ := health.RunCompliance(ctx, f2, health.ComplianceOpts{})
		if !findItem(t, "no_ah_auth", rep2.GetItems()) {
			t.Error("PASS 认证应通过")
		}
	})

	t.Run("virtualization items not applicable", func(t *testing.T) {
		f := newFake()
		rep, _ := health.RunCompliance(ctx, f, health.ComplianceOpts{})
		if !findItem(t, "director_promisc", rep.GetItems()) {
			t.Error("director_promisc 应标记不适用不阻断")
		}
		if !findItem(t, "lacp_ip_hash", rep.GetItems()) {
			t.Error("lacp_ip_hash 应标记不适用不阻断")
		}
	})

	t.Run("time_sync", func(t *testing.T) {
		f := newFake()
		f.outputs[cmdKey("timedatectl", "show", "-p", "NTPSynchronized", "--value")] = "yes"
		rep, _ := health.RunCompliance(ctx, f, health.ComplianceOpts{})
		if !findItem(t, "time_sync", rep.GetItems()) {
			t.Error("NTP 同步应通过")
		}
	})
}

func TestRunFsProbe(t *testing.T) {
	ctx := context.Background()

	t.Run("disk usage threshold", func(t *testing.T) {
		f := newFake()
		f.outputs[cmdKey("df", "-P", "-k", "/etc/nginx")] = "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/sda1 100000 45000 55000 45% /"
		f.outputs[cmdKey("df", "-P", "-k", "/etc")] = "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/sda1 100000 45000 55000 45% /"
		rep, _ := health.RunFsProbe(ctx, f, health.FsProbeOpts{})
		if !findItem(t, "disk_usage_nginx_paths", rep.GetItems()) {
			t.Error("磁盘 45% 应通过")
		}

		f2 := newFake()
		f2.outputs[cmdKey("df", "-P", "-k", "/etc/nginx")] = "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/sda1 100000 95000 5000 95% /"
		f2.outputs[cmdKey("df", "-P", "-k", "/etc")] = "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/sda1 100000 95000 5000 95% /"
		rep2, _ := health.RunFsProbe(ctx, f2, health.FsProbeOpts{})
		if findItem(t, "disk_usage_nginx_paths", rep2.GetItems()) {
			t.Error("磁盘 95% 应判定不通过")
		}
	})

	t.Run("cert expiry", func(t *testing.T) {
		f := newFake()
		rep, _ := health.RunFsProbe(ctx, f, health.FsProbeOpts{})
		if !findItem(t, "cert_expiry", rep.GetItems()) {
			t.Error("无证书目录应跳过不阻断")
		}

		f2 := newFake()
		f2.exists["/etc/nginx/ssl"] = true
		f2.dirs["/etc/nginx/ssl"] = []string{"a.crt"}
		key := cmdKey("openssl", "x509", "-in", "/etc/nginx/ssl/a.crt", "-noout", "-enddate")
		f2.outputs[key] = "notAfter=Dec  5 12:00:00 2027 GMT"
		rep2, _ := health.RunFsProbe(ctx, f2, health.FsProbeOpts{})
		if !findItem(t, "cert_expiry", rep2.GetItems()) {
			t.Error("证书充足有效期应通过")
		}

		f3 := newFake()
		f3.exists["/etc/nginx/ssl"] = true
		f3.dirs["/etc/nginx/ssl"] = []string{"a.crt"}
		key3 := cmdKey("openssl", "x509", "-in", "/etc/nginx/ssl/a.crt", "-noout", "-enddate")
		f3.outputs[key3] = "notAfter=Jan  1 00:00:00 2020 GMT"
		rep3, _ := health.RunFsProbe(ctx, f3, health.FsProbeOpts{})
		if findItem(t, "cert_expiry", rep3.GetItems()) {
			t.Error("证书过期应判定不通过")
		}
	})

	t.Run("config world writable", func(t *testing.T) {
		f := newFake()
		f.exists["/etc/nginx"] = true
		f.exists["/etc/nginx/ssl"] = true
		f.stats["/etc/nginx"] = health.FileInfo{Exists: true, IsDir: true, Mode: os.FileMode(0o755), WorldWritable: false}
		f.stats["/etc/nginx/ssl"] = health.FileInfo{Exists: true, IsDir: true, Mode: os.FileMode(0o755), WorldWritable: false}
		rep, _ := health.RunFsProbe(ctx, f, health.FsProbeOpts{})
		if !findItem(t, "config_world_writable", rep.GetItems()) {
			t.Error("权限非全局可写应通过")
		}

		f2 := newFake()
		f2.exists["/etc/nginx"] = true
		f2.stats["/etc/nginx"] = health.FileInfo{Exists: true, IsDir: true, Mode: os.FileMode(0o777), WorldWritable: true}
		f2.exists["/etc/nginx/ssl"] = true
		f2.stats["/etc/nginx/ssl"] = health.FileInfo{Exists: true, IsDir: true, Mode: os.FileMode(0o755), WorldWritable: false}
		rep2, _ := health.RunFsProbe(ctx, f2, health.FsProbeOpts{})
		if findItem(t, "config_world_writable", rep2.GetItems()) {
			t.Error("全局可写应判定不通过")
		}
	})

	t.Run("log dir writable / pid present", func(t *testing.T) {
		f := newFake()
		f.writable["/var/log/nginx"] = true
		f.exists["/var/run/nginx.pid"] = true
		rep, _ := health.RunFsProbe(ctx, f, health.FsProbeOpts{})
		if !findItem(t, "log_dir_writable", rep.GetItems()) {
			t.Error("日志目录可写应通过")
		}
		if !findItem(t, "pid_file_present", rep.GetItems()) {
			t.Error("pid 文件存在应通过")
		}
	})

	t.Run("error log growth", func(t *testing.T) {
		f := newFake()
		f.exists["/var/log/nginx/error.log"] = true
		var b strings.Builder
		for i := 0; i < 5; i++ {
			b.WriteString("2026/01/01 00:00:00 [error] x: something\n")
		}
		f.outputs[cmdKey("tail", "-n", "1000", "/var/log/nginx/error.log")] = b.String()
		rep, _ := health.RunFsProbe(ctx, f, health.FsProbeOpts{NginxErrorLog: "/var/log/nginx/error.log"})
		if !findItem(t, "error_log_growth", rep.GetItems()) {
			t.Error("少量错误行应通过")
		}

		f2 := newFake()
		f2.exists["/var/log/nginx/error.log"] = true
		var b2 strings.Builder
		for i := 0; i < 300; i++ {
			b2.WriteString("2026/01/01 00:00:00 [error] x: something\n")
		}
		f2.outputs[cmdKey("tail", "-n", "1000", "/var/log/nginx/error.log")] = b2.String()
		rep2, _ := health.RunFsProbe(ctx, f2, health.FsProbeOpts{NginxErrorLog: "/var/log/nginx/error.log"})
		if findItem(t, "error_log_growth", rep2.GetItems()) {
			t.Error("错误雪崩应判定不通过")
		}
	})
}
