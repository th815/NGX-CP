// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T018 · 能力发现（三）：日志采集目标提取。
//
// 目的不是「读日志」，而是回答两个问题：
//  1. 采集目标在哪 —— 从 `nginx -T` 的完整配置里提取所有 access_log / error_log 指令；
//  2. 能不能采集 —— 哪些是 off / syslog（跳过），哪些路径含变量（不展开，告警）。
//
// 设计取舍：
//   - 词法解析复用 internal/pkg/nginxconf，避免各模块各自用 strings.Fields 导致行为不一致；
//   - 变量路径**不做通配展开**：`access_log /var/log/nginx/$host.log` 展开后文件数量不可控，
//     只标记 HasVariable=true 交给平台告警，由人工决定是否改造配置（见 M6 日志方案）；
//   - 只从配置提取路径，不读取 ssl_certificate_key 等敏感文件的内容。
package capability

import (
	"os"
	"sort"
	"strings"
	"syscall"

	"github.com/th/ngxcp/internal/pkg/nginxconf"
)

// 日志目标类型。
const (
	LogTypeAccess = "access"
	LogTypeError  = "error"
)

// LogTarget 是一个日志采集目标。Path 为空表示该指令无需采集（见 SkipReason）。
type LogTarget struct {
	Path        string `json:"path"`                  // /var/log/nginx/access.log
	Type        string `json:"type"`                  // access | error
	Format      string `json:"format,omitempty"`      // main | json ...（仅 access_log 有，未指定则为空即 nginx 默认 combined）
	Level       string `json:"level,omitempty"`       // warn | error ...（仅 error_log 有，未指定则为空即 nginx 默认 error）
	IsSyslog    bool   `json:"is_syslog"`             // syslog:server=... → 由 syslog 采集，跳过
	IsOff       bool   `json:"is_off"`                // 不落盘文件、无需采集：access_log off / error_log stderr / memory
	HasVariable bool   `json:"has_variable"`          // 路径含 $host 等变量 → 不展开，告警
	SkipReason  string `json:"skip_reason,omitempty"` // off | syslog | stderr | memory：不采集的具体原因（UI 直接展示）
	Size        int64  `json:"size"`                  // 运行时 stat 填充，失败为 -1
	Inode       uint64 `json:"inode"`                 // 运行时 stat 填充，用于检测 logrotate 轮转
	StatErr     string `json:"stat_err,omitempty"`    // stat 失败原因（文件不存在 / 权限不足）
}

// access_log 的可选参数前缀，用于区分「格式名」与「选项」。
var accessLogOptPrefixes = []string{"buffer=", "flush=", "if=", "gzip"}

// error_log 的合法级别（nginx 官方文档顺序）。
var errorLogLevels = map[string]bool{
	"debug": true, "info": true, "notice": true, "warn": true,
	"error": true, "crit": true, "alert": true, "emerg": true,
}

// ExtractLogTargets 从 `nginx -T` 解析出的配置树里提取所有日志采集目标。
//
// 处理规则：
//   - `access_log off;`           → IsOff=true，Path 留空（不要把 "off" 当文件名）
//   - `access_log syslog:...;`    → IsSyslog=true，Path 留空
//   - `error_log stderr;|memory:` → 非文件目标，跳过采集（Path 留空并标注）
//   - 路径含 `$`                  → HasVariable=true，保留原路径但不展开
//   - 同（Type, Path）去重，按 Type 再按 Path 排序，保证输出稳定（便于做配置 diff）
//
// 返回的条目 Size/Inode 未填充，需调用 StatLogTargets 在目标主机上补全。
func ExtractLogTargets(files []ConfigFile) []LogTarget {
	var out []LogTarget
	seen := make(map[string]bool)
	for _, f := range files {
		// 必须用字符级扫描器而非按行切分：块头 `http {` 与块内首条指令会被行式累积
		// 粘成一条，导致 server/location 块内的 access_log 全部丢失。
		for _, dir := range nginxconf.ScanDirectives(f.Content) {
			name, args := dir[0], dir[1:]
			var t LogTarget
			switch name {
			case "access_log":
				t = parseAccessLog(args)
			case "error_log":
				t = parseErrorLog(args)
			default:
				continue
			}
			// 也无法归类的目标（路径为空且无跳过原因）→ 丢弃，避免产生无意义条目。
			if t.Path == "" && t.SkipReason == "" {
				continue
			}
			// 去重键带上 SkipReason：error_log 的 stderr / syslog / memory 三者路径皆空，
			// 若仅按 (Type, Path) 去重会被误合并成一条，导致平台看不到 stderr 这类配置。
			key := t.Type + "\x00" + t.Path + "\x00" + t.SkipReason
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// parseAccessLog 解析 access_log 指令参数。
//
//	access_log /path main;
//	access_log /path main buffer=32k flush=5s;
//	access_log /path json gzip[=level] [flush=time];
//	access_log off;
//	access_log syslog:server=10.0.1.5:514,facility=local7;
func parseAccessLog(args []string) LogTarget {
	t := LogTarget{Type: LogTypeAccess, Size: -1}
	if len(args) == 0 {
		return t
	}
	first := args[0]
	switch {
	case first == "off":
		t.IsOff = true
		t.SkipReason = "off"
		return t
	case strings.HasPrefix(first, "syslog:"):
		t.IsSyslog = true
		t.SkipReason = "syslog"
		return t
	}
	t.Path = first
	t.HasVariable = strings.Contains(first, "$")
	// 第二个非选项参数即格式名（nginx 语法保证格式名一定在选项之前）。
	for _, a := range args[1:] {
		if isAccessLogOption(a) {
			continue
		}
		t.Format = a
		break
	}
	return t
}

// parseErrorLog 解析 error_log 指令参数。
//
//	error_log /path warn;
//	error_log /path;              # 默认 error 级别
//	error_log stderr;             # 非文件目标
//	error_log syslog:server=...;
//	error_log memory:32m debug;   # 仅调试用，不落盘
func parseErrorLog(args []string) LogTarget {
	t := LogTarget{Type: LogTypeError, Size: -1}
	if len(args) == 0 {
		return t
	}
	first := args[0]
	switch {
	case strings.HasPrefix(first, "syslog:"):
		t.IsSyslog = true
		t.SkipReason = "syslog"
		return t
	case first == "stderr":
		// 非文件目标：输出到 stderr（通常由 systemd-journald 接管），Agent 不采集。
		t.IsOff = true
		t.SkipReason = "stderr"
		return t
	case strings.HasPrefix(first, "memory:"):
		// 仅调试用的内存环形缓冲，不落盘。
		t.IsOff = true
		t.SkipReason = "memory"
		return t
	}
	t.Path = first
	t.HasVariable = strings.Contains(first, "$")
	if len(args) > 1 && errorLogLevels[args[1]] {
		t.Level = args[1]
	}
	return t
}

// isAccessLogOption 判断参数是否为 access_log 的选项而非格式名。
// `gzip` 可写作 `gzip[=level]`，故用前缀匹配。
func isAccessLogOption(a string) bool {
	for _, p := range accessLogOptPrefixes {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}

// StatLogTargets 在目标主机上对可采集的日志目标执行 stat，补全 Size / Inode。
//
// Inode 是 logrotate 轮转检测的关键：Agent 内置 tail 需靠 inode 变化判断文件是否被
// 重命名重建（否则会一直 tail 已轮转的旧文件）。stat 失败不视为错误——文件尚未产生
// （如新建站点还没流量）是常态，只把原因记到 StatErr 交由平台提示。
func StatLogTargets(targets []LogTarget) []LogTarget {
	for i := range targets {
		t := &targets[i]
		if t.Path == "" || t.IsOff || t.IsSyslog {
			continue
		}
		fi, err := os.Stat(t.Path)
		if err != nil {
			t.StatErr = err.Error()
			continue
		}
		t.Size = fi.Size()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			t.Inode = uint64(st.Ino)
		}
	}
	return targets
}
