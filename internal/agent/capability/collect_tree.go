// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T018 · 能力发现（四）：在受管主机上运行 `nginx -T` 并解析出完整配置树。
//
// 设计要点：
//   - `nginx -T` 把完整配置（含所有 include 进来的文件）打印到 stdout，是「配置树」的
//     唯一权威来源；配置文件含语法错误时 -T 会失败——这是特性而非 bug（见 T017 陷阱，
//     坏配置本就不该进入发布流水线，Agent 侧提前暴露比控制面事后 diff 更有价值）。
//   - 输出解析复用 ParseConfigTree（词法边界切分，严格不串文件）。
//   - 命令经 hostexec 抽象，单测注入 fake 即可，无需真实 nginx。
package capability

import (
	"context"
	"fmt"

	"github.com/th/ngxcp/internal/agent/hostexec"
)

// NginxBin 是采集时执行的 nginx 可执行文件名（PATH 查找，单测由 fake 拦截）。
const NginxBin = "nginx"

// CollectNginxTree 运行 `nginx -T` 并解析出完整配置树（不含私钥等敏感文件内容）。
// 执行失败（如 nginx 未安装、配置语法错误）返回 error；非 nginx 节点不应调用此函数
// （调用方应先确认节点上 nginx 可执行）。
func CollectNginxTree(ctx context.Context, exec hostexec.CommandExecutor) ([]ConfigFile, error) {
	out, err := exec.Output(ctx, NginxBin, "-T")
	if err != nil {
		return nil, fmt.Errorf("执行 nginx -T 失败: %w", err)
	}
	files, err := ParseConfigTree(out)
	if err != nil {
		return nil, fmt.Errorf("解析 nginx -T 输出失败: %w", err)
	}
	return files, nil
}

// CollectNginxV 运行 `nginx -V` 并解析出能力基线画像。
// 注意：-V 把结果写到 stderr，调用方（hostexec.Output 已用 CombinedOutput 合并）。
// 非 nginx 节点（如纯 LVS Director）nginx 不可执行时返回 ErrNginxNotFound。
func CollectNginxV(ctx context.Context, exec hostexec.CommandExecutor) (*NginxInfo, error) {
	out, err := exec.Output(ctx, NginxBin, "-V")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNginxNotFound, err)
	}
	return ParseNginxV(out)
}
