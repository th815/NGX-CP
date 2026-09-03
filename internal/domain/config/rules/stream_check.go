package rules

import (
	"context"
	"fmt"
)

// StreamBlockRule 检查 stream {} 块存在但节点未编译 --with-stream。
// 指令级的 stream 模块缺失由 ModuleCheckRule 统一覆盖，本规则聚焦块级不一致。
type StreamBlockRule struct {
	cfg     *Config
	enabled bool
}

func (r *StreamBlockRule) ID() string      { return "stream_block" }
func (r *StreamBlockRule) Name() string    { return "stream 块编译一致性" }
func (r *StreamBlockRule) Severity() string { return "error" }
func (r *StreamBlockRule) Enabled() bool   { return r.enabled }
func (r *StreamBlockRule) SetEnabled(v bool) { r.enabled = v }

func (r *StreamBlockRule) Check(ctx context.Context, in *CheckInput) []Issue {
	var issues []Issue
	for _, cf := range in.ConfigFiles {
		streams := blocks(cf.Content, "stream")
		if len(streams) == 0 {
			continue
		}
		if !in.Capability.HasModule("stream") {
			nodeName := ""
			if in.Node != nil {
				nodeName = in.Node.Name
			}
			issues = append(issues, Issue{
				RuleID:   r.ID(),
				Severity: "error",
				Message:  fmt.Sprintf("配置包含 stream {} 块，但节点 %q 未编译 --with-stream 模块", nodeName),
				File:     cf.Path,
				Line:     streams[0].Start + 1,
				Fix:      "重新编译 nginx 加入 --with-stream，或移除 stream 块",
			})
		}
	}
	return issues
}
