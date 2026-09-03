// Package capability 实现 Agent 侧能力发现解析：nginx -V（编译参数 → 能力基线）、
// nginx -T（完整配置树）。解析结果由心跳 / 能力上报（T015）带回控制面，
// 供节点详情页「能力基线」Tab 与双机编译一致性检测（M1 集成验收）使用。
//
// 注意：nginx -V 输出到 stderr，调用方必须用 CombinedOutput() 合并后再传入。
package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrNginxNotFound 表示节点上未安装 / 不可执行 nginx（非 nginx 节点，如纯 Director）。
var ErrNginxNotFound = errors.New("nginx 未安装或不可执行")

// NginxInfo 是 `nginx -V` 解析出的能力基线。
type NginxInfo struct {
	Version        string   `json:"version"`        // "1.30.0"
	BinaryPath     string   `json:"binary_path"`    // /usr/sbin/nginx
	ConfigureArgs  string   `json:"configure_args"` // 原始 --prefix=... 整串
	Prefix         string   `json:"prefix"`         // /etc/nginx
	ConfPath       string   `json:"conf_path"`      // /etc/nginx/nginx.conf
	SbinPath       string   `json:"sbin_path"`      // /usr/sbin/nginx
	PidPath        string   `json:"pid_path"`
	LockPath       string   `json:"lock_path"`
	ErrorLogPath   string   `json:"error_log_path"`
	HTTPLogPath    string   `json:"http_log_path"`
	RunUser        string   `json:"run_user"`
	RunGroup       string   `json:"run_group"`
	Compiler       string   `json:"compiler"`
	OpenSSLVersion string   `json:"openssl_version"`
	TLSSNI         bool     `json:"tls_sni"`
	StaticModules  []string `json:"static_modules"`  // http_ssl / stream / nginx_upstream_check_module ...
	DynamicModules []string `json:"dynamic_modules"` // --add-dynamic-module 补
	ConfigHash     string   `json:"config_hash"`     // 基于 ConfigureArgs 的 SHA256
}

var (
	reVersion   = regexp.MustCompile(`nginx version: nginx/([\d.]+)`)
	reOpenSSL   = regexp.MustCompile(`built with OpenSSL ([\d.]+)`)
	reCompiler  = regexp.MustCompile(`built by ([^\n]+)`)
	reSNI       = regexp.MustCompile(`TLS SNI support enabled`)
	reConfigure = regexp.MustCompile(`configure arguments:\s*(.+)`)
)

// ParseNginxV 解析 `nginx -V` 输出（注意：-V 输出到 stderr，调用方应合并后传入）。
// 返回的 NginxInfo 中 StaticModules 已做归一化（--with-http_ssl_module → "http_ssl"，
// --with-stream → "stream"，--add-module=../nginx_upstream_check_module → 取路径末段）。
func ParseNginxV(output string) (*NginxInfo, error) {
	info := &NginxInfo{}
	if m := reVersion.FindStringSubmatch(output); m != nil {
		info.Version = m[1]
	}
	if info.Version == "" {
		return nil, fmt.Errorf("未找到 nginx 版本行，确认输入是 `nginx -V` 的完整输出（含 stderr）")
	}
	if m := reOpenSSL.FindStringSubmatch(output); m != nil {
		info.OpenSSLVersion = m[1]
	}
	if m := reCompiler.FindStringSubmatch(output); m != nil {
		info.Compiler = strings.TrimSpace(m[1])
	}
	info.TLSSNI = reSNI.MatchString(output)

	if m := reConfigure.FindStringSubmatch(output); m != nil {
		info.ConfigureArgs = strings.TrimSpace(m[1])
		parseConfigureArgs(info, info.ConfigureArgs)
	}

	// BinaryPath 与 SbinPath 同源（控制面详情页两者都可能展示）。
	if info.SbinPath != "" && info.BinaryPath == "" {
		info.BinaryPath = info.SbinPath
	}
	sum := sha256.Sum256([]byte(info.ConfigureArgs))
	info.ConfigHash = hex.EncodeToString(sum[:])
	return info, nil
}

// parseConfigureArgs 解析 configure 参数串，填充路径字段与模块清单。
func parseConfigureArgs(info *NginxInfo, args string) {
	for _, a := range splitArgs(args) {
		switch {
		case strings.HasPrefix(a, "--prefix="):
			info.Prefix = val(a)
		case strings.HasPrefix(a, "--conf-path="):
			info.ConfPath = val(a)
		case strings.HasPrefix(a, "--sbin-path="):
			info.SbinPath = val(a)
		case strings.HasPrefix(a, "--pid-path="):
			info.PidPath = val(a)
		case strings.HasPrefix(a, "--lock-path="):
			info.LockPath = val(a)
		case strings.HasPrefix(a, "--error-log-path="):
			info.ErrorLogPath = val(a)
		case strings.HasPrefix(a, "--http-log-path="):
			info.HTTPLogPath = val(a)
		case strings.HasPrefix(a, "--user="):
			info.RunUser = val(a)
		case strings.HasPrefix(a, "--group="):
			info.RunGroup = val(a)
		case strings.HasPrefix(a, "--with-"):
			rest := strings.TrimPrefix(a, "--with-")
			if strings.HasSuffix(rest, "_module") {
				info.StaticModules = append(info.StaticModules, strings.TrimSuffix(rest, "_module"))
			} else {
				// 布尔型模块开关，如 --with-stream / --with-threads / --with-file-aio
				info.StaticModules = append(info.StaticModules, rest)
			}
		case strings.HasPrefix(a, "--add-module="):
			info.StaticModules = append(info.StaticModules, baseName(val(a)))
		case strings.HasPrefix(a, "--add-dynamic-module="):
			info.DynamicModules = append(info.DynamicModules, baseName(val(a)))
		}
	}
}

// val 取 --key=value 中的 value 部分。
func val(arg string) string {
	if i := strings.IndexByte(arg, '='); i >= 0 {
		return arg[i+1:]
	}
	return ""
}

// baseName 取路径末段（兼容 / 与 \ 分隔符），用于 --add-module 提取第三方模块名。
func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// splitArgs 按空白切分参数，但尊重单引号内的空格（如 --with-cc-opt='-g -O2'）。
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}
