// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T060 下发前的可行性检查与计划生成。
//
// 设计立场：**能在下发前发现的问题，绝不留到 `nginx -t` 或真机才暴露**。
// 变更单流水线固然会跑 nginx -t 兜底，但那时用户已经点了发布、等了一轮编排，
// 反馈链路长且脏。这里把三类可预判的失败拦在预览阶段：
//   - nginx 版本不支持 escape=json；
//   - http{} 内没有通配 include，片段落盘后根本不会被加载（最隐蔽：发布"成功"但无日志）；
//   - server/location 层级已有 access_log，会就近覆盖 http 级设置（部分站点静默缺日志）。
package logfmt

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/pkg/nginxconf"
)

// NodePlan 是单节点的下发计划（预览与实际下发共用，保证「所见即所发」）。
type NodePlan struct {
	NodeID      int      `json:"node_id"`
	NginxVer    string   `json:"nginx_version"`
	SnippetPath string   `json:"snippet_path"` // 片段在节点上的落盘路径
	LogPath     string   `json:"log_path"`     // 新增 JSON 日志路径
	FormatName  string   `json:"format_name"`
	IncludeDir  string   `json:"include_dir"` // 由主配置的 include 推导
	Content     string   `json:"content"`     // 将写入的完整片段内容
	Fields      []string `json:"fields"`      // 日志字段清单（与采集侧契约一致）
	Warnings    []string `json:"warnings"`    // 不阻断但必须让人看到的风险
}

// Plan 为单节点生成下发计划；前置条件不满足返回 CodePrecondition，参数非法返回 CodeInvalid。
func (s *Service) Plan(ctx context.Context, nodeID int, opts SnippetOptions) (*NodePlan, error) {
	if nodeID <= 0 {
		return nil, apperr.New(apperr.CodeInvalid, "node_id 必填")
	}
	cap, err := s.nodes.GetCapability(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if cap == nil || cap.Nginx == nil || cap.Nginx.Version == "" {
		return nil, apperr.New(apperr.CodePrecondition,
			fmt.Sprintf("节点 %d 尚无 nginx 能力基线（nginx -V 未采集），请先刷新能力后重试", nodeID))
	}
	if !SupportsEscapeJSON(cap.Nginx.Version) {
		return nil, apperr.New(apperr.CodePrecondition,
			fmt.Sprintf("nginx %s 不支持 escape=json（需 ≥ %s），无法保证 JSON 日志合法性",
				cap.Nginx.Version, MinNginxVersion))
	}

	mainConf, err := s.readMainConf(ctx, nodeID, cap.Nginx.ConfPath)
	if err != nil {
		return nil, err
	}
	incDir, err := resolveIncludeDir(mainConf, cap.Nginx.Prefix)
	if err != nil {
		return nil, err
	}

	// 日志目录跟随存量 access_log，继承既有目录权限与 logrotate 规则。
	if opts.LogPath == "" {
		opts.LogPath = path.Join(pickLogDir(cap.LogTargets), DefaultLogFile)
	}
	opts.Normalize()
	if err := opts.Validate(); err != nil {
		return nil, apperr.New(apperr.CodeInvalid, err.Error())
	}

	p := &NodePlan{
		NodeID:      nodeID,
		NginxVer:    cap.Nginx.Version,
		SnippetPath: path.Join(incDir, SnippetFileName),
		LogPath:     opts.LogPath,
		FormatName:  opts.FormatName,
		IncludeDir:  incDir,
		Content:     RenderSnippet(opts),
		Fields:      FieldKeys(),
	}
	p.Warnings = s.collectWarnings(ctx, nodeID, opts, cap.LogTargets)
	return p, nil
}

// readMainConf 取节点主配置内容（漂移/下发均以平台已同步的版本为准）。
func (s *Service) readMainConf(ctx context.Context, nodeID int, confPath string) (string, error) {
	files, err := s.cfg.ListFiles(ctx, nodeID)
	if err != nil {
		return "", err
	}
	var pick int
	for _, f := range files {
		if confPath != "" && f.Path == confPath {
			pick = f.ID
			break
		}
		// 退化匹配：conf.d 下的同名文件不算主配置。
		if strings.HasSuffix(f.Path, "/nginx.conf") && !strings.Contains(f.Path, "/conf.d/") {
			pick = f.ID
		}
	}
	if pick == 0 {
		return "", apperr.New(apperr.CodePrecondition,
			fmt.Sprintf("平台尚未同步节点 %d 的主配置（nginx.conf），请等待 Agent 上报配置树后重试", nodeID))
	}
	content, err := s.cfg.GetCurrentContent(ctx, pick)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "读取主配置内容失败", err)
	}
	return string(content), nil
}

// resolveIncludeDir 在 http{} 内找一个「通配 include」目录，作为片段落盘位置。
//
// 必须同时满足三点，否则片段不会被加载：
//   - include 位于 http{} 直接层级（在 server/location 里只对该块生效；在 main 层级则
//     log_format 非法）；
//   - 参数是通配（*.conf / *），单文件 include 不会带上新增文件；
//   - 能推导出绝对目录（相对路径按 nginx prefix 解析，与 nginx 自身行为一致）。
//
// 平台不自动改写主配置（「只增不改」约束），因此找不到就明确报错并给出人工修复指引。
func resolveIncludeDir(mainConf, prefix string) (string, error) {
	if prefix == "" {
		prefix = "/etc/nginx"
	}
	var candidates []string
	for _, d := range nginxconf.ScanScoped(mainConf) {
		if d.Name != "include" || len(d.Args) == 0 {
			continue
		}
		if d.Depth() != 1 || !d.InScope("http") {
			continue // main 层级或 server/location 内，均不合用
		}
		pat := d.Args[0]
		if !strings.Contains(path.Base(pat), "*") {
			continue // 单文件 include
		}
		dir := path.Dir(pat)
		if !strings.HasPrefix(dir, "/") {
			dir = path.Join(prefix, dir) // 相对 prefix，与 nginx 一致
		}
		candidates = append(candidates, dir)
	}
	if len(candidates) == 0 {
		return "", apperr.New(apperr.CodePrecondition,
			"未在 http{} 内发现通配 include（如 include /etc/nginx/conf.d/*.conf;）。"+
				"平台当前不改写主配置，请先在 nginx.conf 的 http{} 中加入该 include 并等待配置同步，再下发日志格式")
	}
	// 优先 conf.d（约定俗成，logrotate/SELinux 策略通常已覆盖），否则取第一个。
	for _, c := range candidates {
		if strings.Contains(c, "conf.d") {
			return c, nil
		}
	}
	return candidates[0], nil
}

// pickLogDir 从存量 access_log 推导日志目录：取出现次数最多的目录，
// 让新日志与旧日志同目录（权限、SELinux 上下文、logrotate 通常已就绪）。
func pickLogDir(targets []*node.LogTargetView) string {
	count := map[string]int{}
	for _, t := range targets {
		if t == nil || t.Type != "access" || t.IsOff || t.IsSyslog || t.HasVariable {
			continue
		}
		if p := t.Path; strings.HasPrefix(p, "/") {
			count[path.Dir(p)]++
		}
	}
	best, bestN := "", 0
	for dir, n := range count {
		// 计数相同时按字典序取定，保证同一节点多次预览结果稳定（可比对 diff）。
		if n > bestN || (n == bestN && dir < best) {
			best, bestN = dir, n
		}
	}
	if best == "" {
		return DefaultLogDir
	}
	return best
}

// collectWarnings 汇总不阻断但必须让人看见的风险。
func (s *Service) collectWarnings(ctx context.Context, nodeID int, opts SnippetOptions, targets []*node.LogTargetView) []string {
	var w []string
	// 恒定提示：双写导致磁盘占用增加，且新文件需纳入 logrotate。
	w = append(w, fmt.Sprintf("节点将同时写入存量日志与新增 JSON 日志（%s），磁盘占用相应增加；"+
		"请确认 logrotate 已覆盖该文件（否则会无限增长）", opts.LogPath))

	for _, t := range targets {
		if t == nil || t.Type != "access" {
			continue
		}
		if t.Path == opts.LogPath {
			w = append(w, "该节点已存在同名 JSON 日志目标，本次为幂等重下发（内容一致则不会产生新版本）")
		}
		if t.HasVariable {
			w = append(w, fmt.Sprintf("存量 access_log 路径含变量（%s），这类日志平台不采集，"+
				"其流量只会出现在新增 JSON 日志中", t.Path))
		}
	}
	if ov := s.scanOverrides(ctx, nodeID); len(ov) > 0 {
		w = append(w, fmt.Sprintf("检测到 %d 处 server/location 层级的 access_log（%s）。"+
			"nginx 的 access_log 是就近覆盖而非叠加继承，这些站点不会写入新增 JSON 日志，"+
			"需要为它们单独下发", len(ov), strings.Join(ov, "; ")))
	}
	return w
}

// scanOverrides 扫描节点全部已同步配置，找出会覆盖 http 级设置的 access_log 所在块。
// 读取失败不阻断下发（退化为不告警），因为这只是风险提示而非正确性前提。
func (s *Service) scanOverrides(ctx context.Context, nodeID int) []string {
	files, err := s.cfg.ListFiles(ctx, nodeID)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		content, err := s.cfg.GetCurrentContent(ctx, f.ID)
		if err != nil {
			continue
		}
		for _, d := range nginxconf.ScanScoped(string(content)) {
			if d.Name != "access_log" || len(d.Args) == 0 {
				continue
			}
			if !d.InScope("server") && !d.InScope("location") {
				continue
			}
			key := strings.Join(d.Scope, ">") + " → " + d.Args[0]
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, key)
		}
	}
	sort.Strings(out) // 输出稳定，便于 UI diff 与测试断言
	if len(out) > 5 {
		out = append(out[:5], fmt.Sprintf("... 共 %d 处", len(out)))
	}
	return out
}
