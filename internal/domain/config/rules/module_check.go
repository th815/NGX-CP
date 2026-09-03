package rules

import (
	"context"
	"fmt"
)

// ModuleCheckRule 检查配置用到的指令是否需要特定编译模块，
// 并对比集群内同类节点是否已经编译该模块（提前发现双机不一致）。
type ModuleCheckRule struct {
	cfg     *Config
	enabled bool
}

func (r *ModuleCheckRule) ID() string      { return "module_check" }
func (r *ModuleCheckRule) Name() string    { return "模块编译一致性" }
func (r *ModuleCheckRule) Severity() string { return "error" }
func (r *ModuleCheckRule) Enabled() bool   { return r.enabled }
func (r *ModuleCheckRule) SetEnabled(v bool) { r.enabled = v }

// requiredModules 从所有配置文件中推断需要的编译模块。返回 module -> 首次出现的文件路径。
func (r *ModuleCheckRule) requiredModules(in *CheckInput) map[string]string {
	need := map[string]string{}
	for _, f := range in.ConfigFiles {
		dirs := allDirectives(f.Content)
		if dirs["stream"] {
			// stream 块本身要求 --with-stream
			if _, ok := need["stream"]; !ok {
				need["stream"] = f.Path
			}
		}
		for d := range dirs {
			if mod, ok := r.cfg.ModuleRequirements[d]; ok {
				if _, exists := need[mod]; !exists {
					need[mod] = f.Path
				}
			}
		}
	}
	return need
}

func (r *ModuleCheckRule) Check(ctx context.Context, in *CheckInput) []Issue {
	var issues []Issue
	nodeName := ""
	if in.Node != nil {
		nodeName = in.Node.Name
	}
	need := r.requiredModules(in)
	for mod, path := range need {
		if in.Capability.HasModule(mod) {
			continue
		}
		line := firstLineOf(fileContent(in, path), directiveForModule(r.cfg, mod))
		issues = append(issues, Issue{
			RuleID:   r.ID(),
			Severity: "error",
			Message:  fmt.Sprintf("配置使用了需要模块 %q 的指令，但节点 %q 未编译该模块", mod, nodeName),
			File:     path,
			Line:     line,
			Fix:      fmt.Sprintf("重新编译 nginx 并加入 --with-%s（或 --add-module），或从配置中移除相关指令", mod),
		})
		// 双机漂移：同类节点有该模块而本节点没有 → 额外告警（一台能过一台不能过）。
		for _, p := range in.Peers {
			if p.Role != "" && in.Node != nil && p.Role != in.Node.Role {
				continue
			}
			if p.Cap != nil && p.Cap.HasModule(mod) {
				issues = append(issues, Issue{
					RuleID:   r.ID(),
					Severity: "warning",
					Message:  fmt.Sprintf("双机不一致：同类节点 %q 已编译模块 %q，但本节点 %q 未编译，配置下发后可能出现一台通过一台失败", p.Name, mod, nodeName),
					File:     path,
					Line:     line,
					Fix:      "统一两台 RS 的 nginx 编译参数，确保模块基线一致",
				})
				break
			}
		}
	}
	return issues
}

// directiveForModule 反向查模块需求表，找到映射到该模块的第一个指令名（用于定位行号）。
func directiveForModule(cfg *Config, mod string) string {
	for d, m := range cfg.ModuleRequirements {
		if m == mod {
			return d
		}
	}
	return ""
}

// fileContent 按路径取配置文件内容。
func fileContent(in *CheckInput, path string) string {
	for _, f := range in.ConfigFiles {
		if f.Path == path {
			return f.Content
		}
	}
	return ""
}
