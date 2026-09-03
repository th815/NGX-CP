// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package handler

import (
	"context"

	"github.com/gin-gonic/gin"
	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// ConfigValidator 是「触发 Agent 执行 nginx -t 校验」的抽象（由控制面 gRPC 服务实现）。
// 用接口隔离 HTTP 层与传输层，便于单测注入 fake。
type ConfigValidator interface {
	ValidateConfig(ctx context.Context, nodeID int, task *agentv1.ValidateTask) (*agentv1.ValidateResult, error)
}

// ValidateHandler 处理配置校验 HTTP 入口（T024 nginx -t + T025 语义校验）。
type ValidateHandler struct {
	validator ConfigValidator
	semantic  *config.SemanticChecker
}

// NewValidateHandler 构造校验处理器。
func NewValidateHandler(v ConfigValidator) *ValidateHandler {
	return &ValidateHandler{validator: v}
}

// SetSemanticChecker 注入语义校验器（T025）。未注入时 /semantic-check 返回 5000。
func (h *ValidateHandler) SetSemanticChecker(c *config.SemanticChecker) {
	h.semantic = c
}

type validateFileReq struct {
	Path    string `json:"path"`    // 相对 prefix 的路径，如 "nginx.conf" / "conf.d/api.conf"
	Content string `json:"content"` // 文件原文
}

type validateReq struct {
	NodeID    int              `json:"node_id"`    // 目标节点
	NginxPath string           `json:"nginx_path"` // 可选，默认 /usr/sbin/nginx
	Prefix    string           `json:"prefix"`     // 可选提示（Agent 实际在私有临时目录 staging）
	ConfPath  string           `json:"conf_path"`  // 主配置相对路径，如 "nginx.conf"
	Files     []validateFileReq `json:"files"`      // 完整配置树（主配置 + 所有 include 文件）
}

// Validate 接收待校验配置树，委托 Agent 跑 nginx -t 并返回结果。
//
//	POST /api/v1/configs/validate
//
// 成功：200 {code:0, data:{ok:true, raw, errors:[]}}
// 语法错误：4012 {code:4012, message:"配置语法错误", detail:"nginx: [emerg] ...", data:[...结构化错误]}
// Agent 离线/超时：4103
func (h *ValidateHandler) Validate(c *gin.Context) {
	var in validateReq
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体格式非法").WithDetail(err.Error()))
		return
	}
	if in.NodeID <= 0 {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "node_id 必填"))
		return
	}
	if in.ConfPath == "" || len(in.Files) == 0 {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "conf_path 与 files 必填（校验需完整配置树，不能只校验单文件）"))
		return
	}

	task := &agentv1.ValidateTask{
		NginxPath: in.NginxPath,
		Prefix:    in.Prefix,
		ConfPath:  in.ConfPath,
	}
	for _, f := range in.Files {
		if f.Path == "" {
			response.Fail(c, apperr.New(apperr.CodeInvalid, "files 中存在空路径"))
			return
		}
		task.Files = append(task.Files, &agentv1.ValidateFile{Path: f.Path, Content: f.Content})
	}

	res, err := h.validator.ValidateConfig(c.Request.Context(), in.NodeID, task)
	if err != nil {
		response.Fail(c, err) // Agent 离线/超时 → 4103；其它 → 5000
		return
	}

	vr := config.FromProto(res)
	if verr := vr.ToError(); verr != nil {
		// 校验未通过：以 4012 返回原始输出与结构化错误（含文件/行号）。
		response.FailData(c, verr, res.GetErrors())
		return
	}

	response.OK(c, gin.H{
		"ok":     true,
		"raw":    res.GetRaw(),
		"errors": res.GetErrors(),
	})
}

type semanticCheckReq struct {
	NodeID int `json:"node_id"`
}

// SemanticCheck 对指定节点运行语义校验规则（T025），返回结构化的 Issue 列表。
//
//	POST /api/v1/configs/semantic-check
//
// 成功：200 {code:0, data:{node_id, issues:[...], count}}
func (h *ValidateHandler) SemanticCheck(c *gin.Context) {
	var in semanticCheckReq
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体格式非法").WithDetail(err.Error()))
		return
	}
	if in.NodeID <= 0 {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "node_id 必填"))
		return
	}
	if h.semantic == nil {
		response.Fail(c, apperr.New(apperr.CodeUnavailable, "语义校验器未初始化"))
		return
	}
	issues, err := h.semantic.Check(c.Request.Context(), in.NodeID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"node_id": in.NodeID,
		"issues":  issues,
		"count":   len(issues),
	})
}
