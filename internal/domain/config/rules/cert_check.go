package rules

import (
	"context"
	"fmt"
	"strings"
)

// CertRefRule 检查 ssl_certificate / ssl_certificate_key 引用的证书文件是否存在。
// 节点文件系统不可由控制面直接访问，故以「节点上报的已知文件清单」(KnownFiles) 为准。
type CertRefRule struct {
	cfg     *Config
	enabled bool
}

func (r *CertRefRule) ID() string      { return "cert_ref" }
func (r *CertRefRule) Name() string    { return "证书引用存在性" }
func (r *CertRefRule) Severity() string { return "error" }
func (r *CertRefRule) Enabled() bool   { return r.enabled }
func (r *CertRefRule) SetEnabled(v bool) { r.enabled = v }

func (r *CertRefRule) Check(ctx context.Context, in *CheckInput) []Issue {
	var issues []Issue
	known := map[string]bool{}
	for _, f := range in.KnownFiles {
		known[f] = true
	}
	for _, cf := range in.ConfigFiles {
		for _, s := range statements(cf.Content) {
			if s.name != "ssl_certificate" && s.name != "ssl_certificate_key" {
				continue
			}
			if len(s.args) < 1 {
				continue
			}
			path := strings.Trim(s.args[0], ";\"' ")
			if path == "" {
				continue
			}
			// KnownFiles 为空代表无法判定，跳过（不误报）。
			if len(in.KnownFiles) == 0 {
				continue
			}
			if !known[path] {
				issues = append(issues, Issue{
					RuleID:   r.ID(),
					Severity: "error",
					Message:  fmt.Sprintf("证书指令 %s 引用的文件不存在：%s", s.name, path),
					File:     cf.Path,
					Line:     s.line,
					Fix:      "确认证书已上传到该节点对应路径，或修正 ssl_certificate 路径",
				})
			}
		}
	}
	return issues
}
