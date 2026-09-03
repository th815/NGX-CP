// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package config

import (
	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// ValidateResult 是控制面侧对 Agent 校验结果的标准化领域视图。
type ValidateResult struct {
	OK     bool
	Raw    string                 // nginx -t 原始输出
	Errors []*agentv1.NginxError  // 结构化错误（OK=false 时有内容）
}

// FromProto 把 Agent 经心跳流回传的 proto 校验结果转为领域视图。
func FromProto(res *agentv1.ValidateResult) *ValidateResult {
	if res == nil {
		return &ValidateResult{}
	}
	return &ValidateResult{OK: res.GetOk(), Raw: res.GetRaw(), Errors: res.GetErrors()}
}

// ToError 把未通过的校验结果转为业务错误：
// 未通过 → CodePrecondition(4012)「配置语法错误」，detail 携带 nginx 原始输出；
// 通过（OK=true）→ 返回 nil。
// 注意：bind() 端口被占用（Address already in use）在 Agent 侧已归为 warn 级，
// 但 nginx -t 整体仍 failed，故 OK=false；调用方可据 Errors 中的 level 进一步区分运行时/语法问题。
func (r *ValidateResult) ToError() error {
	if r.OK {
		return nil
	}
	return apperr.New(apperr.CodePrecondition, "配置语法错误").WithDetail(r.Raw)
}
