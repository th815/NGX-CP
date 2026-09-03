package rules

import (
	"context"
	"fmt"
	"strings"
)

// DRPortRule 检查 DR 模式下的常见误配：真实服务器（real_server）直接在 listen 中绑定
// 集群 VIP。LVS-DR 要求 VIP 只配在回环 lo:0 上，RS 的 nginx 应监听 0.0.0.0/物理地址，
// 误绑 VIP 会导致 ARP 冲突与流量错乱。VIP 列表来自 cfg.DR.VIPs，为空则跳过。
type DRPortRule struct {
	cfg     *Config
	enabled bool
}

func (r *DRPortRule) ID() string      { return "dr_port" }
func (r *DRPortRule) Name() string    { return "DR 模式 VIP 绑定" }
func (r *DRPortRule) Severity() string { return "warning" }
func (r *DRPortRule) Enabled() bool   { return r.enabled }
func (r *DRPortRule) SetEnabled(v bool) { r.enabled = v }

func (r *DRPortRule) Check(ctx context.Context, in *CheckInput) []Issue {
	if len(r.cfg.DR.VIPs) == 0 {
		return nil
	}
	if in.Node != nil && in.Node.Role != "real_server" && in.Node.Role != "director_and_rs" {
		return nil
	}
	var issues []Issue
	for _, cf := range in.ConfigFiles {
		for _, sb := range blocks(cf.Content, "server") {
			for _, s := range stmtsInBlock(cf.Content, sb) {
				if s.name != "listen" || len(s.args) < 1 {
					continue
				}
				hostPort := strings.Trim(s.args[0], ";\"' ")
				if i := strings.IndexAny(hostPort, " \t"); i >= 0 {
					hostPort = hostPort[:i]
				}
				host := hostPort
				if strings.Contains(hostPort, ":") {
					host = strings.SplitN(hostPort, ":", 2)[0]
				}
				for _, vip := range r.cfg.DR.VIPs {
					if host == vip {
						issues = append(issues, Issue{
							RuleID:   r.ID(),
							Severity: "warning",
							Message:  fmt.Sprintf("DR 模式下 RS 不应直接 listen VIP %s，VIP 应仅配在 lo:0，否则会引发 ARP 冲突", vip),
							File:     cf.Path,
							Line:     s.line,
							Fix:      "将 listen 改为 0.0.0.0:port 或物理接口地址，VIP 仅在 lo:0 上配置",
						})
					}
				}
			}
		}
	}
	return issues
}
