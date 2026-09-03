// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T018 · 能力发现（三）：主机系统信息采集与原子落盘可行性检查。
//
// 两块职责：
//  1. SystemInfo —— 纳管节点的运行底座画像（OS/内核/托管方式/SELinux/ulimit/时区/
//     NTP/磁盘余量/logrotate）。这既是节点详情页「系统信息」Tab 的数据源，也是
//     发布前的环境校验依据（如 ulimit 过低、SELinux enforcing 会阻断发布）。
//  2. AtomicWriteCheck —— 判断配置目录与暂存目录是否同一文件系统。
//     这是「原子落盘」的地基：只有同设备内的 rename(2) 才是原子的，跨设备 rename
//     会退化为「copy + 校验 + 切换」，需要加文件锁并告警（见 T017 陷阱）。
//
// 设计取舍：采集为**尽力而为**（best-effort）——某个命令缺失或失败（如没装
// SELinux、没有 timedatectl）只留零值并记到 Warnings，绝不整体失败。
// 系统信息是辅助决策，不该因为一台机器的 getenforce 不存在就让能力上报失败。
package capability

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/th/ngxcp/internal/agent/hostexec"
)

// 托管方式与 SELinux 状态的取值。
const (
	ManagedBySystemd = "systemd"
	ManagedByManual  = "manual"

	SELinuxEnforcing  = "enforcing"
	SELinuxPermissive = "permissive"
	SELinuxDisabled   = "disabled"
	SELinuxUnknown    = "unknown" // getenforce 不存在（未安装 SELinux）
)

// SystemInfo 是主机的运行底座画像。所有字段均为尽力采集，取不到时为零值。
type SystemInfo struct {
	OS             string           `json:"os"`                 // "rocky 9.4"（取自 os-release 的 ID + VERSION_ID）
	Kernel         string           `json:"kernel"`             // uname -r
	NginxManagedBy string           `json:"nginx_managed_by"`   // systemd | manual
	SELinuxStatus  string           `json:"selinux_status"`     // enforcing | permissive | disabled | unknown
	UlimitNofile   int              `json:"ulimit_nofile"`      // 进程可打开文件数上限
	Timezone       string           `json:"timezone"`           // Asia/Shanghai
	NTPSynced      bool             `json:"ntp_synced"`         // 时间是否已同步（chrony/ntp）
	ClockSkew      time.Duration    `json:"clock_skew_ns"`      // 由调用方按控制面时间填充，此处恒为 0
	LogRotateConf  string           `json:"logrotate_conf"`     // /etc/logrotate.d/nginx 存在则为该路径，否则空
	DiskFree       map[string]int64 `json:"disk_free"`          // 挂载点 -> 可用字节
	Warnings       []string         `json:"warnings,omitempty"` // 采集失败的项（平台据此提示，不算节点故障）
}

// AtomicWriteCheck 原子落盘可行性检查结果。
type AtomicWriteCheck struct {
	ConfDir         string `json:"conf_dir"`
	StagingDir      string `json:"staging_dir"`
	SameDevice      bool   `json:"same_device"` // ★ 同设备才能用 rename 原子切换
	ConfDeviceID    uint64 `json:"conf_device_id"`
	StagingDeviceID uint64 `json:"staging_device_id"`
	ConfWritable    bool   `json:"conf_writable"`    // 配置目录可写（Agent 通常以 root 运行）
	StagingWritable bool   `json:"staging_writable"` // 暂存目录可写
	Degraded        bool   `json:"degraded"`         // true 表示需降级为「copy + 校验 + 切换」
	Reason          string `json:"reason,omitempty"` // 降级原因（UI 直接展示）
}

// 采集所用的命令与路径常量（集中定义，便于跨平台替换与测试）。
const (
	pathOSRelease  = "/etc/os-release"
	pathTimezone   = "/etc/timezone"
	pathLogRotate  = "/etc/logrotate.d/nginx"
	cmdSystemctl   = "systemctl"
	cmdGetenforce  = "getenforce"
	cmdTimedatectl = "timedatectl"
	cmdUname       = "uname"
	cmdShell       = "sh"
)

// CollectSystemInfo 在目标主机采集系统信息（只读，尽力而为）。
// exec 抽象了命令执行，单测注入 fake 即可覆盖各发行版差异，无需真机。
func CollectSystemInfo(ctx context.Context, exec hostexec.CommandExecutor) SystemInfo {
	var (
		info SystemInfo
		warn = func(what string) {
			info.Warnings = append(info.Warnings, what)
		}
	)

	// OS：优先 /etc/os-release（systemd 生态标准），回退 uname -srm。
	if raw, err := exec.ReadFile(pathOSRelease); err == nil {
		info.OS = ParseOSRelease(raw)
	}
	if info.OS == "" {
		if out, err := exec.Output(ctx, cmdUname, "-srm"); err == nil && strings.TrimSpace(out) != "" {
			info.OS = strings.TrimSpace(out)
		} else {
			warn("os_release")
		}
	}

	if out, err := exec.Output(ctx, cmdUname, "-r"); err == nil {
		info.Kernel = strings.TrimSpace(out)
	} else {
		warn("kernel")
	}

	// 托管方式：systemctl is-enabled 能返回 enabled/disabled 即由 systemd 托管；
	// 命令不存在（非 systemd 发行版或容器）或返回其他值都按 manual 处理。
	switch out, err := exec.Output(ctx, cmdSystemctl, "is-enabled", "nginx"); {
	case err != nil:
		info.NginxManagedBy = ManagedByManual
	case strings.HasPrefix(strings.TrimSpace(out), "enabled"),
		strings.HasPrefix(strings.TrimSpace(out), "disabled"),
		strings.HasPrefix(strings.TrimSpace(out), "static"),
		strings.HasPrefix(strings.TrimSpace(out), "indirect"):
		info.NginxManagedBy = ManagedBySystemd
	default:
		info.NginxManagedBy = ManagedByManual
	}

	// SELinux：getenforce 不存在即未安装，按 unknown（而非 disabled）——
	// 「没装」与「装了但关闭」对发布的影响不同，不应混为一谈。
	switch out, err := exec.Output(ctx, cmdGetenforce); {
	case err != nil:
		info.SELinuxStatus = SELinuxUnknown
		warn("selinux")
	default:
		switch strings.ToLower(strings.TrimSpace(out)) {
		case "enforcing":
			info.SELinuxStatus = SELinuxEnforcing
		case "permissive":
			info.SELinuxStatus = SELinuxPermissive
		case "disabled":
			info.SELinuxStatus = SELinuxDisabled
		default:
			info.SELinuxStatus = SELinuxUnknown
		}
	}

	// ulimit -n 是 shell 内建命令，必须经 sh -c 调用。
	if out, err := exec.Output(ctx, cmdShell, "-c", "ulimit -n"); err == nil {
		if n, e := strconv.Atoi(strings.TrimSpace(out)); e == nil {
			info.UlimitNofile = n
		} else {
			warn("ulimit")
		}
	} else {
		warn("ulimit")
	}

	// 时区：/etc/timezone（Debian 系）优先，回退 timedatectl。
	if raw, err := exec.ReadFile(pathTimezone); err == nil && strings.TrimSpace(raw) != "" {
		info.Timezone = strings.TrimSpace(raw)
	} else if out, err := exec.Output(ctx, cmdTimedatectl, "show", "-p", "Timezone", "--value"); err == nil {
		if tz := strings.TrimSpace(out); tz != "" {
			info.Timezone = tz
		}
	}
	if info.Timezone == "" {
		warn("timezone")
	}

	// NTP 同步：timedatectl 为准，回退 chronyc tracking（用户环境用 chrony）。
	if out, err := exec.Output(ctx, cmdTimedatectl, "show", "-p", "NTPSynchronized", "--value"); err == nil {
		info.NTPSynced = strings.EqualFold(strings.TrimSpace(out), "yes")
	} else if out, err := exec.Output(ctx, "chronyc", "tracking"); err == nil {
		info.NTPSynced = strings.Contains(strings.ToLower(out), "leap status") &&
			!strings.Contains(strings.ToLower(out), "not synchronised")
	} else {
		warn("ntp")
	}

	if exec.Exists(pathLogRotate) {
		info.LogRotateConf = pathLogRotate
	} else {
		warn("logrotate")
	}

	// 命令失败与「成功但解析不出任何挂载点」都算采集失败——后者常见于容器内 df 输出被裁剪，
	// 静默放过会让平台误以为「磁盘余量未知」无需关注，而它其实是发布前必须确认的项。
	out, err := exec.Output(ctx, "df", "-B1", "-P")
	info.DiskFree = ParseDF(out)
	if err != nil || len(info.DiskFree) == 0 {
		warn("disk")
		if info.DiskFree == nil {
			info.DiskFree = map[string]int64{}
		}
	}
	return info
}

// ParseOSRelease 从 /etc/os-release 内容提取 "ID VERSION_ID"。
// 例：ID="rocky" + VERSION_ID="9.4" → "rocky 9.4"。取不到返回空串（由调用方回退 uname）。
func ParseOSRelease(raw string) string {
	var id, version string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "ID":
			id = v
		case "VERSION_ID":
			version = v
		}
	}
	switch {
	case id != "" && version != "":
		return id + " " + version
	case id != "":
		return id
	default:
		return ""
	}
}

// ParseDF 解析 `df -B1 -P` 输出，返回「挂载点 -> 可用字节」。
// 用 -P（POSIX 输出格式）保证每行一条记录、长设备名不折行，挂载点取该行剩余全部内容
// （挂载点可能含空格，不能只取 fields[5]）。
func ParseDF(out string) map[string]int64 {
	res := make(map[string]int64)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Filesystem") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		avail, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		mount := strings.Join(fields[5:], " ")
		res[mount] = avail
	}
	return res
}

// CheckAtomicWrite 检查配置目录与暂存目录能否用 rename 原子切换。
// 这是发布引擎的前置校验：同设备 + 两处可写 → 可原子；否则必须降级并告警。
func CheckAtomicWrite(confDir, stagingDir string) (*AtomicWriteCheck, error) {
	return checkAtomicWrite(hostexec.NewRealExecutor(), hostexec.DeviceID, confDir, stagingDir)
}

// checkAtomicWrite 是 CheckAtomicWrite 的可注入版本（exec 与设备号查询均可替换，便于单测）。
func checkAtomicWrite(exec hostexec.CommandExecutor, devID func(string) uint64, confDir, stagingDir string) (*AtomicWriteCheck, error) {
	c := &AtomicWriteCheck{
		ConfDir:         confDir,
		StagingDir:      stagingDir,
		ConfDeviceID:    devID(confDir),
		StagingDeviceID: devID(stagingDir),
		ConfWritable:    exec.IsWritableDir(confDir),
		StagingWritable: exec.IsWritableDir(stagingDir),
	}
	// 设备号为 0 表示取不到（路径不存在或平台不支持）——此时不能判定为同设备，
	// 否则会把「无法确认」误报为「可原子」，是发布事故的高危误判方向。
	c.SameDevice = c.ConfDeviceID != 0 && c.ConfDeviceID == c.StagingDeviceID

	switch {
	case !c.SameDevice:
		c.Degraded = true
		c.Reason = "配置目录与暂存目录不在同一文件系统，rename 不原子，将降级为「copy + 校验 + 切换」并加文件锁"
	case !c.StagingWritable:
		c.Degraded = true
		c.Reason = "暂存目录不可写，无法准备待发布配置"
	case !c.ConfWritable:
		c.Degraded = true
		c.Reason = "配置目录不可写；若 SELinux 为 enforcing，请检查文件上下文"
	}
	return c, nil
}
