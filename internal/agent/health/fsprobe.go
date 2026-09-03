// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 tianhao

package health

import (
	"context"
	"fmt"
	"github.com/th/ngxcp/internal/agent/hostexec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/domain/probe"
)

// FsProbeOpts 控制日志/文件系统健康探测的参数（nginx 路径来自能力发现，其余为阈值）。
type FsProbeOpts struct {
	NginxPrefix    string // nginx --prefix，默认 /etc/nginx
	NginxConfPath  string // nginx.conf 绝对路径
	NginxErrorLog  string // error_log 路径（用于错误速率与日志目录判定）
	NginxPidPath   string // nginx.pid 路径，默认 /var/run/nginx.pid
	SslDir         string // 证书目录，默认 /etc/nginx/ssl
	DiskWarnPct    int    // 磁盘使用率告警阈值（%），默认 90
	CertMinDays    int    // 证书最小剩余天数，默认 14
	ErrorWindowMin int    // 错误速率统计窗口（分钟），默认 5
}

func (o *FsProbeOpts) defaults() {
	if o.NginxPrefix == "" {
		o.NginxPrefix = "/etc/nginx"
	}
	if o.NginxConfPath == "" {
		o.NginxConfPath = "/etc/nginx/nginx.conf"
	}
	if o.NginxPidPath == "" {
		o.NginxPidPath = "/var/run/nginx.pid"
	}
	if o.SslDir == "" {
		o.SslDir = "/etc/nginx/ssl"
	}
	if o.DiskWarnPct <= 0 {
		o.DiskWarnPct = 90
	}
	if o.CertMinDays <= 0 {
		o.CertMinDays = 14
	}
	if o.ErrorWindowMin <= 0 {
		o.ErrorWindowMin = 5
	}
}

// probeRule 是 probe.RuleDef 的别名（字段与 complianceRule 一致）。
type probeRule = probe.RuleDef

// RunFsProbe 在主机上执行日志/文件系统健康探测，返回控制面约定的 FsProbeReport。
// 规则目录复用 internal/domain/probe.Catalog。
func RunFsProbe(ctx context.Context, exec hostexec.CommandExecutor, opts FsProbeOpts) (*agentv1.FsProbeReport, error) {
	opts.defaults()
	items := make([]*agentv1.ComplianceItem, 0, len(probe.Catalog))
	for _, rule := range probe.Catalog {
		it := &agentv1.ComplianceItem{
			Name:     rule.Name,
			Title:    rule.Title,
			Severity: rule.Severity,
			Expected: rule.Expected,
			FixCmd:   rule.FixCmd,
		}
		switch rule.Name {
		case "disk_usage_nginx_paths":
			checkDiskUsage(exec, opts, it)
		case "cert_expiry":
			checkCertExpiry(exec, opts, it)
		case "config_world_writable":
			checkConfigWritable(exec, opts, it)
		case "log_dir_writable":
			checkLogDirWritable(exec, opts, it)
		case "error_log_growth":
			checkErrorLogGrowth(exec, opts, it)
		case "pid_file_present":
			checkPidFile(exec, opts, it)
		}
		items = append(items, it)
	}
	return &agentv1.FsProbeReport{
		CheckedAt: time.Now().Unix(),
		Items:     items,
	}, nil
}

// checkDiskUsage 检查 nginx 相关挂载点（prefix/conf/log）磁盘使用率 < 阈值。
func checkDiskUsage(exec hostexec.CommandExecutor, opts FsProbeOpts, it *agentv1.ComplianceItem) {
	paths := []string{opts.NginxPrefix, filepath.Dir(opts.NginxConfPath)}
	if opts.NginxErrorLog != "" {
		paths = append(paths, filepath.Dir(opts.NginxErrorLog))
	}
	maxPct := 0
	failed := false
	var details []string
	for _, p := range paths {
		out, err := exec.Output(context.Background(), "df", "-P", "-k", p)
		if err != nil {
			details = append(details, fmt.Sprintf("%s: 探测失败 %v", p, err))
			failed = true
			continue
		}
		pct, perr := parseDfUsage(out)
		if perr != nil {
			details = append(details, fmt.Sprintf("%s: 解析失败 %v", p, perr))
			failed = true
			continue
		}
		if pct > maxPct {
			maxPct = pct
		}
		details = append(details, fmt.Sprintf("%s=%d%%", p, pct))
	}
	if failed {
		it.Passed = false
		it.Actual = strings.Join(details, "; ")
		return
	}
	if maxPct >= opts.DiskWarnPct {
		it.Passed = false
		it.Actual = fmt.Sprintf("最大挂载点使用率 %d%% ≥ %d%%（%s）", maxPct, opts.DiskWarnPct, strings.Join(details, "; "))
	} else {
		it.Passed = true
		it.Actual = fmt.Sprintf("最大挂载点使用率 %d%% < %d%%", maxPct, opts.DiskWarnPct)
	}
}

// parseDfUsage 从 `df -P -k` 输出解析使用率百分比（第 5 列）。
func parseDfUsage(out string) (int, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("df 输出不足两行")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 5 {
		return 0, fmt.Errorf("df 字段不足")
	}
	pctStr := strings.TrimSuffix(fields[4], "%")
	return strconv.Atoi(pctStr)
}

// reEnddate 匹配 openssl x509 -enddate 输出中的 notAfter 字段。
var reEnddate = regexp.MustCompile(`notAfter=\s*(.+)`)

// reErrLevel 匹配 nginx error.log 中的错误级别关键字。
var reErrLevel = regexp.MustCompile(`(?i)\b(error|crit|emerg|alert)\b`)

// checkCertExpiry 扫描证书目录，检查全部证书剩余有效期 > 阈值（与 PKI 续签联动）。
func checkCertExpiry(exec hostexec.CommandExecutor, opts FsProbeOpts, it *agentv1.ComplianceItem) {
	if !exec.Exists(opts.SslDir) {
		it.Passed = true
		it.Actual = "未找到证书目录（节点暂不托管证书），跳过"
		return
	}
	files := listCertFiles(exec, opts.SslDir)
	if len(files) == 0 {
		it.Passed = true
		it.Actual = "证书目录无可识别证书文件，跳过"
		return
	}
	tooSoon := make([]string, 0)
	for _, f := range files {
		out, err := exec.Output(context.Background(), "openssl", "x509", "-in", f, "-noout", "-enddate")
		if err != nil {
			tooSoon = append(tooSoon, fmt.Sprintf("%s: 读取失败 %v", f, err))
			continue
		}
		m := reEnddate.FindStringSubmatch(out)
		if m == nil {
			tooSoon = append(tooSoon, f+": 无法解析到期日")
			continue
		}
		exp, perr := time.Parse("Jan 2 15:04:05 2006 MST", strings.TrimSpace(m[1]))
		if perr != nil {
			tooSoon = append(tooSoon, f+": 日期解析失败")
			continue
		}
		days := int(time.Until(exp).Hours() / 24)
		if days < opts.CertMinDays {
			tooSoon = append(tooSoon, fmt.Sprintf("%s 剩 %d 天", f, days))
		}
	}
	if len(tooSoon) == 0 {
		it.Passed = true
		it.Actual = fmt.Sprintf("全部证书剩余有效期 ≥ %d 天", opts.CertMinDays)
	} else {
		it.Passed = false
		it.Actual = "存在临近过期证书: " + strings.Join(tooSoon, "; ")
	}
}

// listCertFiles 列出目录下的证书文件（.crt/.pem/.cert）。
func listCertFiles(exec hostexec.CommandExecutor, dir string) []string {
	names, err := exec.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		lower := strings.ToLower(n)
		if strings.HasSuffix(lower, ".crt") || strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".cert") {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out
}

// checkConfigWritable 检查 /etc/nginx 与 ssl 目录权限非全局可写（防越权篡改）。
func checkConfigWritable(exec hostexec.CommandExecutor, opts FsProbeOpts, it *agentv1.ComplianceItem) {
	targets := []string{opts.NginxPrefix, opts.SslDir}
	bad := make([]string, 0)
	for _, t := range targets {
		if !exec.Exists(t) {
			continue
		}
		fi, err := exec.Stat(t)
		if err != nil {
			bad = append(bad, t+": 无法 stat")
			continue
		}
		if fi.WorldWritable {
			bad = append(bad, fmt.Sprintf("%s 权限 %o 全局可写", t, fi.Mode.Perm()))
		}
	}
	if len(bad) == 0 {
		it.Passed = true
		it.Actual = "配置与证书目录权限非全局可写"
	} else {
		it.Passed = false
		it.Actual = strings.Join(bad, "; ")
	}
}

// checkLogDirWritable 检查 nginx 日志目录可写（否则启动失败或丢日志）。
func checkLogDirWritable(exec hostexec.CommandExecutor, opts FsProbeOpts, it *agentv1.ComplianceItem) {
	dir := "/var/log/nginx"
	if opts.NginxErrorLog != "" {
		dir = filepath.Dir(opts.NginxErrorLog)
	}
	if exec.IsWritableDir(dir) {
		it.Passed = true
		it.Actual = fmt.Sprintf("%s 可写", dir)
	} else {
		it.Passed = false
		it.Actual = fmt.Sprintf("%s 不可写", dir)
	}
}

// checkErrorLogGrowth 检查 error.log 近窗口内错误级别行数是否雪崩（早期预警，warning 级不阻断）。
func checkErrorLogGrowth(exec hostexec.CommandExecutor, opts FsProbeOpts, it *agentv1.ComplianceItem) {
	if opts.NginxErrorLog == "" {
		it.Passed = true
		it.Actual = "未配置 error_log 路径，跳过"
		return
	}
	if !exec.Exists(opts.NginxErrorLog) {
		it.Passed = true
		it.Actual = "error.log 不存在，跳过"
		return
	}
	out, err := exec.Output(context.Background(), "tail", "-n", "1000", opts.NginxErrorLog)
	if err != nil {
		it.Passed = true
		it.Actual = "读取 error.log 失败，按 warning 不阻断: " + err.Error()
		return
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if reErrLevel.MatchString(line) {
			count++
		}
	}
	threshold := 200 // 近 1000 行错误条数阈值（宽松，仅早期预警）
	if count > threshold {
		it.Passed = false
		it.Actual = fmt.Sprintf("近 1000 行 error 级别 %d 条 > %d，疑似错误雪崩", count, threshold)
	} else {
		it.Passed = true
		it.Actual = fmt.Sprintf("近 1000 行 error 级别 %d 条 ≤ %d", count, threshold)
	}
}

// checkPidFile 检查 nginx pid 文件存在（运行态正常性参考，warning 级不阻断）。
func checkPidFile(exec hostexec.CommandExecutor, opts FsProbeOpts, it *agentv1.ComplianceItem) {
	if exec.Exists(opts.NginxPidPath) {
		it.Passed = true
		it.Actual = fmt.Sprintf("%s 存在", opts.NginxPidPath)
	} else {
		it.Passed = false
		it.Actual = fmt.Sprintf("%s 不存在（nginx 可能未运行）", opts.NginxPidPath)
	}
}
