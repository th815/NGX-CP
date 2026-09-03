package rules

import (
	"context"
	"fmt"
	"strings"
)

// SecurityRule 检查常见安全配置疏漏：
//   - server_tokens on（信息泄露）
//   - location 暴露 .git/.env 等敏感路径
//   - 监听 SSL 的 server 未显式设置 ssl_protocols，或协议低于阈值
type SecurityRule struct {
	cfg     *Config
	enabled bool
}

func (r *SecurityRule) ID() string      { return "security" }
func (r *SecurityRule) Name() string    { return "安全基线" }
func (r *SecurityRule) Severity() string { return "warning" }
func (r *SecurityRule) Enabled() bool   { return r.enabled }
func (r *SecurityRule) SetEnabled(v bool) { r.enabled = v }

func (r *SecurityRule) Check(ctx context.Context, in *CheckInput) []Issue {
	var issues []Issue
	for _, cf := range in.ConfigFiles {
		for _, sb := range blocks(cf.Content, "server") {
			issues = append(issues, r.checkServer(cf.Path, cf.Content, sb)...)
		}
	}
	return issues
}

func (r *SecurityRule) checkServer(file, content string, sb block) []Issue {
	var issues []Issue
	hasSSL := false
	hasProto := false
	minRank := tlsRank(r.cfg.Security.MinTLS)
	for _, s := range stmtsInBlock(content, sb) {
		switch s.name {
		case "server_tokens":
			if len(s.args) > 0 && s.args[0] == "on" {
				issues = append(issues, Issue{
					RuleID:   r.ID(),
					Severity: "warning",
					Message:  "server_tokens 为 on，会泄露 nginx 版本信息",
					File:     file,
					Line:     s.line,
					Fix:      "设置 server_tokens off;",
				})
			}
		case "listen":
			if len(s.args) > 0 && strings.Contains(s.args[0], "ssl") {
				hasSSL = true
			}
		case "ssl_protocols":
			hasProto = true
			if minRank > 0 {
				if rank := tlsRankOfLine(s.args); rank >= 0 && rank < minRank {
					issues = append(issues, Issue{
						RuleID:   r.ID(),
						Severity: "warning",
						Message:  fmt.Sprintf("ssl_protocols 包含低于 %s 的协议版本", r.cfg.Security.MinTLS),
						File:     file,
						Line:     s.line,
						Fix:      fmt.Sprintf("设置 ssl_protocols %s;", r.cfg.Security.MinTLS),
					})
				}
			}
		case "location":
			if r.cfg.Security.ForbidDotGitLocation && locationExposes(s.args, r.cfg.Security.ForbiddenPaths) {
				issues = append(issues, Issue{
					RuleID:   r.ID(),
					Severity: "warning",
					Message:  "location 暴露了敏感路径（如 .git/.env），存在信息泄露风险",
					File:     file,
					Line:     s.line,
					Fix:      "移除或限制该 location 的访问权限",
				})
			}
		}
	}
	if hasSSL && !hasProto {
		issues = append(issues, Issue{
			RuleID:   r.ID(),
			Severity: "warning",
			Message:  "监听 SSL 但未显式设置 ssl_protocols，将回退到不安全默认值",
			File:     file,
			Line:     sb.Start,
			Fix:      fmt.Sprintf("设置 ssl_protocols %s;", r.cfg.Security.MinTLS),
		})
	}
	return issues
}

// locationExposes 判断 location 参数是否命中任一敏感路径片段。
func locationExposes(args []string, forbidden []string) bool {
	if len(args) < 1 {
		return false
	}
	path := strings.Trim(args[0], ";\"' ")
	if path == "/" {
		return false
	}
	for _, f := range forbidden {
		if strings.Contains(path, f) {
			return true
		}
	}
	return false
}

// tlsRank 返回 TLS 版本排序值（越高越安全）；未知返回 -1。
func tlsRank(v string) int {
	switch strings.ToUpper(v) {
	case "TLSv1.3":
		return 4
	case "TLSv1.2":
		return 3
	case "TLSv1.1":
		return 2
	case "TLSv1":
		return 1
	case "SSLv3", "SSLV3":
		return 0
	default:
		return -1
	}
}

// tlsRankOfLine 取 ssl_protocols 参数中最低（最弱）版本的 rank。
func tlsRankOfLine(args []string) int {
	lowest := 99
	for _, tok := range args {
		tok = strings.Trim(tok, ";\"'")
		rk := tlsRank(tok)
		if rk >= 0 && rk < lowest {
			lowest = rk
		}
	}
	if lowest == 99 {
		return -1
	}
	return lowest
}
