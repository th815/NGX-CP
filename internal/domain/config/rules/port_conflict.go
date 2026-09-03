package rules

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// PortConflictRule 检测 server 块间 listen 端口冲突：
//  1. 完全相同的 addr:port 被多个 server 同时绑定（nginx 直接拒绝）；
//  2. 同一端口一个声明 ssl 一个未声明（平面 vs TLS 混用，监听语义冲突）。
type PortConflictRule struct {
	cfg     *Config
	enabled bool
}

func (r *PortConflictRule) ID() string      { return "port_conflict" }
func (r *PortConflictRule) Name() string    { return "端口冲突" }
func (r *PortConflictRule) Severity() string { return "error" }
func (r *PortConflictRule) Enabled() bool   { return r.enabled }
func (r *PortConflictRule) SetEnabled(v bool) { r.enabled = v }

type listenSpec struct {
	addr string
	ssl  bool
	line int
	file string
}

func (r *PortConflictRule) Check(ctx context.Context, in *CheckInput) []Issue {
	var issues []Issue
	seen := map[string][]listenSpec{}
	for _, cf := range in.ConfigFiles {
		for _, sb := range blocks(cf.Content, "server") {
			for _, s := range stmtsInBlock(cf.Content, sb) {
				if s.name != "listen" || len(s.args) < 1 {
					continue
				}
				ls := parseListen(s.args[0], s.line, cf.Path)
				if ls == nil {
					continue
				}
				seen[ls.addr] = append(seen[ls.addr], *ls)
			}
		}
	}
	for addr, specs := range seen {
		if len(specs) < 2 {
			continue
		}
		sslSet := map[bool]bool{}
		for _, s := range specs {
			sslSet[s.ssl] = true
		}
		first := specs[0]
		others := strings.Builder{}
		for _, s := range specs[1:] {
			fmt.Fprintf(&others, " %s:%d", s.file, s.line)
		}
		if sslSet[true] && sslSet[false] {
			issues = append(issues, Issue{
				RuleID:   r.ID(),
				Severity: "error",
				Message:  fmt.Sprintf("端口 %s 同时被平面监听与 SSL 监听混用（nginx 将拒绝加载）", addr),
				File:     first.file,
				Line:     first.line,
				Fix:      "统一该端口的 SSL 设置：要么都加 ssl，要么使用不同端口",
			})
		} else {
			issues = append(issues, Issue{
				RuleID:   r.ID(),
				Severity: "error",
				Message:  fmt.Sprintf("地址 %s 被多个 server 块重复监听（重复 bind）", addr),
				File:     first.file,
				Line:     first.line,
				Fix:      "为每个 server 使用唯一的 listen 地址/端口" + others.String(),
			})
		}
	}
	return issues
}

// parseListen 解析 listen 参数，返回归一化 addr（host:port）与是否 ssl。
func parseListen(arg string, lineNo int, file string) *listenSpec {
	raw := strings.Trim(arg, ";\"' ")
	ssl := strings.Contains(raw, "ssl")
	// 去掉参数部分（default_server / ssl / backlog=...）
	hostPort := raw
	if i := strings.IndexAny(raw, " \t"); i >= 0 {
		hostPort = raw[:i]
	}
	host, port := "*", "80"
	if strings.Contains(hostPort, ":") {
		parts := strings.SplitN(hostPort, ":", 2)
		host = parts[0]
		if host == "" {
			host = "*"
		}
		port = parts[1]
	} else if isIP(hostPort) {
		host = hostPort
	} else {
		port = hostPort // 仅端口，如 listen 80;
	}
	return &listenSpec{addr: host + ":" + port, ssl: ssl, line: lineNo, file: file}
}

func isIP(s string) bool {
	return net.ParseIP(s) != nil
}
