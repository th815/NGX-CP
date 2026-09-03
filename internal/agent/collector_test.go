// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T018-C 采集器单测：用 fake hostexec 注入预置命令输出，验证 nginx -T / -V / 系统信息采集
// 经 Collector 正确解析并映射为 proto 报告（无需真实 nginx 或 root）。
package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent/hostexec"
)

// fakeExec 实现 hostexec.CommandExecutor，按命令名+参数返回预置输出。
type fakeExec struct {
	out    map[string]string
	errs   map[string]error
	files  map[string]string
	exists map[string]bool
}

func (f *fakeExec) Output(_ context.Context, name string, args ...string) (string, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if e, ok := f.errs[key]; ok {
		return "", e
	}
	return f.out[key], nil
}
func (f *fakeExec) Stat(path string) (hostexec.FileInfo, error) {
	if _, ok := f.exists[path]; ok {
		return hostexec.FileInfo{Exists: true, IsDir: false}, nil
	}
	return hostexec.FileInfo{}, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
}
func (f *fakeExec) Exists(p string) bool  { return f.exists[p] }
func (f *fakeExec) IsWritableDir(p string) bool { return true }
func (f *fakeExec) ReadDir(_ string) ([]string, error) { return nil, nil }
func (f *fakeExec) ReadFile(p string) (string, error) { return f.files[p], nil }

const nginxTDump = `# configuration file /etc/nginx/nginx.conf:
user nginx;
error_log /var/log/nginx/error.log warn;
http {
    access_log /var/log/nginx/access.log main;
    access_log /var/log/nginx/vhost.access.log json;
    access_log off;
}
# configuration file /etc/nginx/conf.d/default.conf:
server {
    error_log syslog:server=10.0.0.1:514;
}
`

const nginxV = `nginx version: nginx/1.30.0
built by gcc 11.2.0
built with OpenSSL 3.0.7
configure arguments: --prefix=/etc/nginx --conf-path=/etc/nginx/nginx.conf --with-http_v3_module --with-stream --add-module=/opt/nginx_upstream_check_module`

func newFakeExec() *fakeExec {
	return &fakeExec{
		out: map[string]string{
			"nginx -T": nginxTDump,
			"nginx -V": nginxV,
			"uname -r": "5.14.0-th",
			"df -B1 -P": "Filesystem 1K-blocks Used Available Use% Mounted on\n/dev/sda1 100 50 50 50% /\n",
			"systemctl is-enabled nginx": "enabled",
			"getenforce":                 "Enforcing",
			"sh -c ulimit -n":            "1024",
			"timedatectl show -p Timezone --value": "Asia/Shanghai",
			"timedatectl show -p NTPSynchronized --value": "yes",
		},
		errs: map[string]error{
			"chronyc tracking": os.ErrInvalid, // 触发回退，由 timedatectl 提供 NTP
		},
		files: map[string]string{
			"/etc/os-release":            "ID=\"rocky\"\nVERSION_ID=\"9.4\"\n",
			"/etc/timezone":              "Asia/Shanghai",
			"/etc/logrotate.d/nginx":     "# managed\n",
		},
		exists: map[string]bool{
			"/etc/os-release":           true,
			"/etc/timezone":             true,
			"/etc/logrotate.d/nginx":    true,
			"/var/log/nginx/access.log": true,
		},
	}
}

// findTarget 在报告中按路径查找日志目标（测试辅助）。
func findTarget(t *testing.T, items []*agentv1.LogTarget, path string) *agentv1.LogTarget {
	t.Helper()
	for _, it := range items {
		if it.Path == path {
			return it
		}
	}
	return nil
}

func TestCollectConfigTree(t *testing.T) {
	c := NewCollector(newFakeExec(), nil)
	rep, err := c.CollectConfigTree(context.Background())
	if err != nil {
		t.Fatalf("CollectConfigTree: %v", err)
	}
	if len(rep.Files) != 2 {
		t.Fatalf("配置树文件数 = %d, want 2（nginx.conf + default.conf）", len(rep.Files))
	}
	if rep.Files[0].Path != "/etc/nginx/nginx.conf" {
		t.Errorf("首文件应为 nginx.conf, 实际 %s", rep.Files[0].Path)
	}
	if rep.Files[0].Size <= 0 || rep.Files[0].Sha256 == "" {
		t.Errorf("文件元数据未填充: %+v", rep.Files[0])
	}
}

func TestCollectLogTargets(t *testing.T) {
	c := NewCollector(newFakeExec(), nil)
	rep, err := c.CollectLogTargets(context.Background())
	if err != nil {
		t.Fatalf("CollectLogTargets: %v", err)
	}
	// access.log（main）、vhost.access.log（json）、off 三项来自 access_log。
	acc := findTarget(t, rep.Items, "/var/log/nginx/access.log")
	if acc == nil {
		t.Fatal("未提取 /var/log/nginx/access.log")
	}
	if acc.Type != "access" || acc.Format != "main" {
		t.Errorf("access.log 解析错误: %+v", acc)
	}
	vhost := findTarget(t, rep.Items, "/var/log/nginx/vhost.access.log")
	if vhost == nil || vhost.Format != "json" {
		t.Errorf("vhost.access.log 解析错误: %+v", vhost)
	}
	// off 目标：路径为空、SkipReason=off。
	var off *agentv1.LogTarget
	for _, it := range rep.Items {
		if it.SkipReason == "off" {
			off = it
		}
	}
	if off == nil || !off.IsOff {
		t.Errorf("access_log off 未识别为跳过目标: %+v", off)
	}
	// syslog 目标：error_log 的 syslog 写法应识别为 is_syslog。
	var sys *agentv1.LogTarget
	for _, it := range rep.Items {
		if it.IsSyslog {
			sys = it
		}
	}
	if sys == nil {
		t.Error("error_log syslog 未识别为 syslog 目标")
	}
}

func TestCollectCapability(t *testing.T) {
	c := NewCollector(newFakeExec(), nil)
	cap, err := c.CollectCapability(context.Background())
	if err != nil {
		t.Fatalf("CollectCapability: %v", err)
	}
	if cap.GetNginx() == nil {
		t.Fatal("nginx 画像缺失")
	}
	if cap.GetNginx().GetVersion() != "1.30.0" {
		t.Errorf("版本 = %s, want 1.30.0", cap.GetNginx().GetVersion())
	}
	if !contains(cap.GetNginx().GetStaticModules(), "http_v3") {
		t.Errorf("静态模块含 http_v3, 实际 %v", cap.GetNginx().GetStaticModules())
	}
	if !contains(cap.GetNginx().GetStaticModules(), "stream") {
		t.Errorf("静态模块含 stream, 实际 %v", cap.GetNginx().GetStaticModules())
	}
	si := cap.GetSystem()
	if si == nil {
		t.Fatal("系统信息缺失")
	}
	if si.GetOs() != "rocky 9.4" {
		t.Errorf("OS = %q, want rocky 9.4", si.GetOs())
	}
	if si.GetSelinuxStatus() != "enforcing" {
		t.Errorf("SELinux = %q, want enforcing", si.GetSelinuxStatus())
	}
	if !si.GetNtpSynced() {
		t.Error("NTP 应为已同步")
	}
	if si.GetUlimitNofile() != 1024 {
		t.Errorf("ulimit = %d, want 1024", si.GetUlimitNofile())
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
