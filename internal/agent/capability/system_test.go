// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package capability

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/th/ngxcp/internal/agent/hostexec"
)

// fakeExec 实现 hostexec.CommandExecutor，按命令/路径返回预置结果。
type fakeExec struct {
	outputs  map[string]string // key: "name arg1 arg2"
	readFile map[string]string
	exists   map[string]bool
	writable map[string]bool
	fail     map[string]bool // 该命令执行失败（模拟命令不存在）
}

func newFakeExec() *fakeExec {
	return &fakeExec{
		outputs:  map[string]string{},
		readFile: map[string]string{},
		exists:   map[string]bool{},
		writable: map[string]bool{},
		fail:     map[string]bool{},
	}
}

func key(name string, args ...string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func (f *fakeExec) Output(_ context.Context, name string, args ...string) (string, error) {
	k := key(name, args...)
	if f.fail[k] {
		return "", os.ErrNotExist
	}
	return f.outputs[k], nil
}
func (f *fakeExec) Stat(string) (hostexec.FileInfo, error) { return hostexec.FileInfo{}, nil }
func (f *fakeExec) Exists(p string) bool                   { return f.exists[p] }
func (f *fakeExec) IsWritableDir(p string) bool            { return f.writable[p] }
func (f *fakeExec) ReadDir(string) ([]string, error)       { return nil, nil }
func (f *fakeExec) ReadFile(p string) (string, error) {
	s, ok := f.readFile[p]
	if !ok {
		return "", os.ErrNotExist
	}
	return s, nil
}

func TestParseOSRelease(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"Rocky", "NAME=\"Rocky Linux\"\nID=\"rocky\"\nVERSION_ID=\"9.4\"\nPRETTY_NAME=\"Rocky Linux 9.4 (Blue Onyx)\"\n", "rocky 9.4"},
		{"Ubuntu", "ID=ubuntu\nVERSION_ID=\"22.04\"\n", "ubuntu 22.04"},
		{"单引号", "ID='debian'\nVERSION_ID='12'\n", "debian 12"},
		{"仅ID", "ID=alpine\n", "alpine"},
		{"空", "# 只有注释\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseOSRelease(c.in); got != c.want {
				t.Errorf("ParseOSRelease = %q, 期望 %q", got, c.want)
			}
		})
	}
}

func TestParseDF(t *testing.T) {
	out := `Filesystem         1B-blocks          Used     Available Capacity Mounted on
/dev/mapper/rl-root 107374182400   53687091200  53687091200      50% /
/dev/sda1             1073741824      104857600    968884224      10% /boot
tmpfs                  1073741824             0    1073741824       0% /dev/shm
/dev/sdb1        10737418240000  5368709120000 5368709120000      50% /mnt/data with space
`
	got := ParseDF(out)
	want := map[string]int64{
		"/":                    53687091200,
		"/boot":                968884224,
		"/dev/shm":             1073741824,
		"/mnt/data with space": 5368709120000, // 挂载点含空格时必须完整保留
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseDF =\n%v\n期望\n%v", got, want)
	}

	// 表头与畸形行必须被跳过，不得 panic 或产生垃圾条目。
	got = ParseDF("Filesystem 1B-blocks Used Available Capacity Mounted on\nbad line\n\n")
	if len(got) != 0 {
		t.Errorf("畸形输入应返回空 map，实际 %v", got)
	}
}

func TestCollectSystemInfo(t *testing.T) {
	f := newFakeExec()
	f.readFile["/etc/os-release"] = "ID=\"rocky\"\nVERSION_ID=\"9.4\"\n"
	f.readFile["/etc/timezone"] = "Asia/Shanghai\n"
	f.exists["/etc/logrotate.d/nginx"] = true
	f.outputs[key("uname", "-r")] = "5.14.0-427.el9.x86_64\n"
	f.outputs[key("systemctl", "is-enabled", "nginx")] = "enabled\n"
	f.outputs[key("getenforce")] = "Enforcing\n"
	f.outputs[key("sh", "-c", "ulimit -n")] = "1024000\n"
	f.outputs[key("timedatectl", "show", "-p", "NTPSynchronized", "--value")] = "yes\n"
	f.outputs[key("df", "-B1", "-P")] = "Filesystem 1B-blocks Used Available Capacity Mounted on\n/dev/sda1 100 40 60 40% /\n"

	info := CollectSystemInfo(context.Background(), f)

	if info.OS != "rocky 9.4" {
		t.Errorf("OS = %q", info.OS)
	}
	if info.Kernel != "5.14.0-427.el9.x86_64" {
		t.Errorf("Kernel = %q", info.Kernel)
	}
	if info.NginxManagedBy != ManagedBySystemd {
		t.Errorf("NginxManagedBy = %q, 期望 systemd", info.NginxManagedBy)
	}
	if info.SELinuxStatus != SELinuxEnforcing {
		t.Errorf("SELinuxStatus = %q, 期望 enforcing", info.SELinuxStatus)
	}
	if info.UlimitNofile != 1024000 {
		t.Errorf("UlimitNofile = %d", info.UlimitNofile)
	}
	if info.Timezone != "Asia/Shanghai" {
		t.Errorf("Timezone = %q", info.Timezone)
	}
	if !info.NTPSynced {
		t.Error("NTPSynced 应为 true")
	}
	if info.LogRotateConf != "/etc/logrotate.d/nginx" {
		t.Errorf("LogRotateConf = %q", info.LogRotateConf)
	}
	if info.DiskFree["/"] != 60 {
		t.Errorf("DiskFree = %v", info.DiskFree)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("全部采集成功时不应有 Warnings: %v", info.Warnings)
	}
}

// TestCollectSystemInfoDegrades 命令缺失时降级为零值 + Warnings，绝不整体失败。
func TestCollectSystemInfoDegrades(t *testing.T) {
	f := newFakeExec() // 无任何预置：所有命令与文件都不存在
	f.fail[key("getenforce")] = true
	f.fail[key("timedatectl", "show", "-p", "NTPSynchronized", "--value")] = true
	f.fail[key("chronyc", "tracking")] = true

	info := CollectSystemInfo(context.Background(), f)

	if info.SELinuxStatus != SELinuxUnknown {
		t.Errorf("getenforce 缺失应为 unknown，实际 %q", info.SELinuxStatus)
	}
	if info.NTPSynced {
		t.Error("NTP 命令缺失时 NTPSynced 应为 false")
	}
	if info.NginxManagedBy != ManagedByManual {
		t.Errorf("systemctl 缺失应为 manual，实际 %q", info.NginxManagedBy)
	}
	for _, w := range []string{"selinux", "ntp", "logrotate", "disk"} {
		if !contains(info.Warnings, w) {
			t.Errorf("Warnings 应包含 %q，实际 %v", w, info.Warnings)
		}
	}
}

// TestCheckAtomicWriteSameDevice 真实文件系统：同一 /tmp 下两目录必然同设备且可写。
func TestCheckAtomicWriteSameDevice(t *testing.T) {
	base := t.TempDir()
	conf := filepath.Join(base, "conf")
	stage := filepath.Join(base, "staging")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := CheckAtomicWrite(conf, stage)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SameDevice {
		t.Errorf("同目录下两子目录应同设备，实际 conf=%d staging=%d", got.ConfDeviceID, got.StagingDeviceID)
	}
	if !got.ConfWritable || !got.StagingWritable {
		t.Error("两个目录都应可写")
	}
	if got.Degraded {
		t.Errorf("不应降级，Reason=%q", got.Reason)
	}
}

// TestCheckAtomicWriteCrossDevice 跨设备（模拟不同 st_dev）必须降级，
// 这是发布引擎能否用 rename 的关键判定。
func TestCheckAtomicWriteCrossDevice(t *testing.T) {
	f := newFakeExec()
	f.writable["/etc/nginx"] = true
	f.writable["/var/lib/ngxcp/staging"] = true

	got, err := checkAtomicWrite(f, func(p string) uint64 {
		if p == "/etc/nginx" {
			return 66306
		}
		return 66307
	}, "/etc/nginx", "/var/lib/ngxcp/staging")
	if err != nil {
		t.Fatal(err)
	}
	if got.SameDevice {
		t.Error("不同设备号不应判定为 SameDevice")
	}
	if !got.Degraded || got.Reason == "" {
		t.Error("跨设备必须降级并给出原因")
	}
}

// TestCheckAtomicWriteUnwritable 目录不可写必须降级（SELinux/权限问题的典型表现）。
func TestCheckAtomicWriteUnwritable(t *testing.T) {
	f := newFakeExec()
	f.writable["/etc/nginx"] = true
	f.writable["/var/lib/ngxcp/staging"] = false // 暂存目录不可写

	got, err := checkAtomicWrite(f, func(string) uint64 { return 66306 }, "/etc/nginx", "/var/lib/ngxcp/staging")
	if err != nil {
		t.Fatal(err)
	}
	if !got.SameDevice {
		t.Error("同设备号应判定为 SameDevice")
	}
	if !got.Degraded || !strings.Contains(got.Reason, "暂存目录") {
		t.Errorf("暂存目录不可写应降级并说明原因，实际 Reason=%q", got.Reason)
	}
}

// TestCheckAtomicWriteDeviceUnknown 设备号取不到（0）时必须按「不可原子」处理——
// 把「无法确认」误报成「可原子」是发布事故的高危方向。
func TestCheckAtomicWriteDeviceUnknown(t *testing.T) {
	f := newFakeExec()
	f.writable["/etc/nginx"] = true
	f.writable["/var/lib/ngxcp/staging"] = true

	got, err := checkAtomicWrite(f, func(string) uint64 { return 0 }, "/etc/nginx", "/var/lib/ngxcp/staging")
	if err != nil {
		t.Fatal(err)
	}
	if got.SameDevice {
		t.Error("设备号未知时不得判定为 SameDevice")
	}
	if !got.Degraded {
		t.Error("设备号未知时应降级")
	}
}

// TestSystemInfoClockSkewDefault 时钟偏差由控制面按心跳时间戳计算，采集侧不填充。
func TestSystemInfoClockSkewDefault(t *testing.T) {
	var info SystemInfo
	if info.ClockSkew != time.Duration(0) {
		t.Errorf("采集侧 ClockSkew 应恒为 0，实际 %v", info.ClockSkew)
	}
}
