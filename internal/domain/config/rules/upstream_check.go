package rules

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// UpstreamReachRule 检查 upstream 结构完整性：
//  1. upstream 块内没有任何 server → nginx 会报错；
//  2. proxy_pass 引用了未声明的 upstream；
//  3.（可选）对 upstream server 的域名做一次 DNS 解析探测（默认关闭，避免网络抖动）。
type UpstreamReachRule struct {
	cfg     *Config
	enabled bool
}

func (r *UpstreamReachRule) ID() string      { return "upstream_reach" }
func (r *UpstreamReachRule) Name() string    { return "upstream 可达性" }
func (r *UpstreamReachRule) Severity() string { return "error" }
func (r *UpstreamReachRule) Enabled() bool   { return r.enabled }
func (r *UpstreamReachRule) SetEnabled(v bool) { r.enabled = v }

func (r *UpstreamReachRule) Check(ctx context.Context, in *CheckInput) []Issue {
	var issues []Issue
	upstreams := map[string]block{}   // name -> 块范围
	hosts := map[string]string{}       // name -> 首个 server 的 host

	for _, cf := range in.ConfigFiles {
		for _, ub := range blocks(cf.Content, "upstream") {
			name := ""
			serverCount := 0
			firstHost := ""
			for _, s := range stmtsInBlock(cf.Content, ub) {
				switch s.name {
				case "upstream":
					if len(s.args) > 0 {
						name = s.args[0]
					}
				case "server":
					serverCount++
					if firstHost == "" && len(s.args) > 0 {
						firstHost = upstreamHost(s.args[0])
					}
				}
			}
			if name == "" {
				continue
			}
			upstreams[name] = ub
			hosts[name] = firstHost
			if serverCount == 0 {
				issues = append(issues, Issue{
					RuleID:   r.ID(),
					Severity: "error",
					Message:  fmt.Sprintf("upstream %q 内没有任何 server 指令，nginx 将拒绝加载", name),
					File:     cf.Path,
					Line:     ub.Start,
					Fix:      "为该 upstream 至少添加一个 server 后端地址",
				})
			}
		}
	}

	// 收集所有 proxy_pass 引用的 upstream 名。
	for _, cf := range in.ConfigFiles {
		for _, s := range statements(cf.Content) {
			if s.name != "proxy_pass" || len(s.args) < 1 {
				continue
			}
			target := strings.Trim(s.args[0], ";\"' ")
			name := upstreamRefName(target)
			if name == "" {
				continue
			}
			if _, ok := upstreams[name]; !ok {
				issues = append(issues, Issue{
					RuleID:   r.ID(),
					Severity: "error",
					Message:  fmt.Sprintf("proxy_pass 引用了未声明的 upstream：%s", name),
					File:     cf.Path,
					Line:     s.line,
					Fix:      "检查 upstream 名称拼写，或补充对应的 upstream 块",
				})
			}
		}
	}

	// 可选 DNS 探测（默认关闭）。
	if r.cfg.Upstream.ResolveDNS {
		for name, host := range hosts {
			if host == "" || net.ParseIP(host) != nil {
				continue
			}
			ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := net.DefaultResolver.LookupHost(ctx2, host)
			cancel()
			if err != nil {
				issues = append(issues, Issue{
					RuleID:   r.ID(),
					Severity: "warning",
					Message:  fmt.Sprintf("upstream %q 的后端域名 %q 无法解析（可达性未知）：%v", name, host, err),
					Fix:      "确认 DNS 解析与后端服务可用性",
				})
			}
		}
	}
	return issues
}

// upstreamRefName 从 proxy_pass 值中提取 upstream 名（形如 http://svc / https://svc）。
func upstreamRefName(target string) string {
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return ""
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(target, "https://"), "http://")
	if strings.Contains(rest, "$") {
		return "" // 变量，无法静态判定
	}
	rest = strings.SplitN(rest, "/", 2)[0]
	rest = strings.SplitN(rest, ":", 2)[0]
	return rest
}

// upstreamHost 取 server 参数的 host（去掉端口）。
func upstreamHost(arg string) string {
	h := strings.Trim(arg, ";\"' ")
	h = strings.SplitN(h, ":", 2)[0]
	return h
}
