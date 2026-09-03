// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package executor 实现 Agent 在受管主机上执行的能力型命令。
// 当前落地 T024 的 nginx -t 配置语法校验。
//
// 设计约束（软件著作权审查与可测试性）：
//   - 命令执行经 CommandRunner 抽象：真实运行用 hostexec.CommandExecutor.Output，
//     单测用 fake 注入预置输出，无需真实 nginx 二进制；
//   - 校验必须在 staging 目录里以完整上下文运行 `nginx -t -p <prefix> -c <conf>`，
//     绝不能单独校验单个 conf 文件（否则 include 相对路径会解析错，导致误判）。
package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CommandRunner 抽象在主机上执行命令的能力（与 hostexec.CommandExecutor.Output 对齐）。
type CommandRunner interface {
	// Output 执行命令并返回合并的 stdout+stderr 输出。
	Output(ctx context.Context, name string, args ...string) (string, error)
}

// ValidateRequest 是一次校验请求（纯数据，便于单测注入）。
// Files 的 key 是相对 prefix 的路径（如 "nginx.conf" / "conf.d/api.conf"），
// value 是文件原文；Agent 会按该相对结构原样写入 staging 目录。
type ValidateRequest struct {
	NginxPath string            // nginx 二进制路径；空则默认 /usr/sbin/nginx
	Prefix    string            // -p 参数：staging 根目录（文件相对它布局，保证 include 相对路径语义一致）
	ConfPath  string            // 主配置相对 prefix 的路径，如 "nginx.conf"
	Files     map[string]string // 相对 prefix 的路径 -> 文件原文
}

// NginxError 是 nginx -t 输出的单条结构化错误。
type NginxError struct {
	Level   string // emerg | alert | crit | error | warn | notice | info
	Message string // 不含文件路径/行号前缀的纯消息
	File    string // 出错文件绝对路径（来自 nginx 输出），无则为空
	Line    int    // 出错行号，无则为 0
}

// ValidateResponse 是校验结果。
type ValidateResponse struct {
	OK     bool          // 语法是否通过（nginx 打印 "test is successful"）
	Errors []NginxError  // 结构化错误（OK=false 时含真实错误；bind 端口占用等运行时问题归为 warn）
	Raw    string        // nginx -t 原始输出
}

// Executor 在节点上跑 nginx -t 校验。
type Executor struct {
	run CommandRunner
}

// NewExecutor 用真实命令执行器构造（Agent 运行时使用 hostexec）。
func NewExecutor(c CommandRunner) *Executor {
	return &Executor{run: c}
}

// NewExecutorWithRunner 便于单测注入自定义执行函数。
func NewExecutorWithRunner(run func(ctx context.Context, name string, args ...string) (string, error)) *Executor {
	return &Executor{run: runnerFunc(run)}
}

type runnerFunc func(ctx context.Context, name string, args ...string) (string, error)

func (f runnerFunc) Output(ctx context.Context, name string, args ...string) (string, error) {
	return f(ctx, name, args...)
}

// reErrLine 匹配 `nginx: [level] 剩余消息` 行，把整条消息（含可能内嵌的 "in"）原样取出。
var reErrLine = regexp.MustCompile(`^nginx:\s+\[(\w+)\]\s+(.+)$`)

// reFileLine 从消息尾部贪婪锁定最后一个 ` in <path>:<line>`（path 不含空白，nginx 路径惯例），
// 避免消息正文里的 "in"（如 `host not found in upstream "x" in /etc/nginx/...:5`）被误判为文件定位。
var reFileLine = regexp.MustCompile(`^(.*)\s+in\s+(\S+):(\d+)\s*$`)

// Validate 在 staging 目录里以完整上下文校验配置。
// 流程：① 写文件到 staging（保持目录结构）→ ② `nginx -t -p prefix -c conf`
// → ③ 解析合并输出。nginx -t 失败会返回非零退出码，但输出仍可用，故非零退出不视为错误，
// 仅当完全没有输出且命令本身失败时返回 error（如 nginx 二进制缺失）。
func (e *Executor) Validate(ctx context.Context, req ValidateRequest) (*ValidateResponse, error) {
	if req.ConfPath == "" {
		return nil, fmt.Errorf("校验请求缺少主配置路径 ConfPath")
	}
	nginxPath := req.NginxPath
	if nginxPath == "" {
		nginxPath = "/usr/sbin/nginx"
	}
	prefix := req.Prefix
	if prefix == "" {
		// 未指定 prefix 时退化为系统临时目录下的 staging（仍保持文件相对结构）。
		tmp, err := os.MkdirTemp("", "ngxcp-validate-")
		if err != nil {
			return nil, fmt.Errorf("创建 staging 目录失败: %w", err)
		}
		defer os.RemoveAll(tmp)
		prefix = tmp
	}

	if err := writeStaging(prefix, req.Files); err != nil {
		return nil, err
	}

	confArg := filepath.Join(prefix, req.ConfPath)
	raw, runErr := e.run.Output(ctx, nginxPath, "-t", "-p", prefix, "-c", confArg)
	// nginx -t 在配置有问题时退出码非 0，但输出已写入 raw，这里不直接当作失败。
	if raw == "" && runErr != nil {
		return nil, fmt.Errorf("执行 nginx -t 失败: %w", runErr)
	}
	return parseValidateOutput(raw), nil
}

// writeStaging 按相对结构把文件写入 staging 根目录。
func writeStaging(root string, files map[string]string) error {
	for rel, content := range files {
		target := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", filepath.Dir(target), err)
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			return fmt.Errorf("写入文件 %s 失败: %w", target, err)
		}
	}
	return nil
}

// parseValidateOutput 解析 nginx -t 的合并输出，结构化为 ValidateResponse。
// 关键判定：
//   - OK = 输出含 "test is successful"（nginx 成功时的标准收尾行）；
//   - 逐行解析 `nginx: [level] message [in file:line]`；
//   - bind() 端口被占用（Address already in use）是运行时问题而非配置语法错误，
//     归为 warn 且不计入致命错误（见 T024 契约陷阱）。
func parseValidateOutput(raw string) *ValidateResponse {
	resp := &ValidateResponse{Raw: raw, OK: strings.Contains(raw, "test is successful")}
	for _, line := range strings.Split(raw, "\n") {
		m := reErrLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		level := strings.ToLower(m[1])
		full := strings.TrimSpace(m[2])
		ne := NginxError{Level: level, Message: full}
		// 从消息尾部提取 ` in <path>:<line>` 文件定位（若有）。
		if fm := reFileLine.FindStringSubmatch(full); fm != nil {
			ne.Message = strings.TrimSpace(fm[1])
			ne.File = fm[2]
			if _, err := fmt.Sscanf(fm[3], "%d", &ne.Line); err != nil {
				ne.Line = 0
			}
		}
		// 端口被占用：运行时问题，降级为 warn，不阻断（已有 nginx 占用端口是正常现象）。
		if strings.Contains(strings.ToLower(ne.Message), "address already in use") {
			ne.Level = "warn"
		}
		resp.Errors = append(resp.Errors, ne)
	}
	return resp
}
