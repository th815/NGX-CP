// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package agent

import (
	"context"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/agent/executor"
	"github.com/th/ngxcp/internal/agent/hostexec"
)

// RunValidateTask 是 HeartbeatCallbacks.ValidateConfig 的默认实现：
// 在节点本地私有临时目录里 staging 待校验文件，跑 `nginx -t -p <staging> -c <staging>/<conf>`，
// 并把结构化结果转回 proto。绝不写入/读取节点真实 /etc/nginx，避免污染线上配置。
//
// 注意：校验以「完整配置树」为单位（主配置 + 所有 include 文件随 task.Files 一并提供），
// 因此相对路径的 include 能在 staging 内正确解析。绝对路径 include（如 include /etc/nginx/mime.types）
// 在当前实现下会指向真实文件——若其存在则无碍，否则 nginx -t 报缺文件；后续可由控制面在
// task 中一并携带该文件内容来覆盖（T028 配置下发时统一处理）。
func RunValidateTask(ctx context.Context, task *agentv1.ValidateTask) (*agentv1.ValidateResult, error) {
	files := make(map[string]string, len(task.GetFiles()))
	for _, f := range task.GetFiles() {
		files[f.GetPath()] = f.GetContent()
	}
	req := executor.ValidateRequest{
		NginxPath: task.GetNginxPath(),
		// Prefix 置空：交由 executor 在本地建私有临时目录作 -p，避免触碰真实 prefix。
		Prefix:   "",
		ConfPath: task.GetConfPath(),
		Files:    files,
	}

	res, err := executor.NewExecutor(hostexec.NewRealExecutor()).Validate(ctx, req)
	if err != nil {
		return nil, err
	}

	out := &agentv1.ValidateResult{
		TaskId: task.GetTaskId(),
		Ok:     res.OK,
		Raw:    res.Raw,
	}
	for _, e := range res.Errors {
		out.Errors = append(out.Errors, &agentv1.NginxError{
			Level:   e.Level,
			Message: e.Message,
			File:    e.File,
			Line:    int64(e.Line),
		})
	}
	return out, nil
}
