// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T060 标准 JSON 日志格式下发的 HTTP 端点。
//
// 三个端点对应「先看契约 → 再看这台机器会被改成什么样 → 才允许发」的操作节奏：
//   - GET  /logs/format          契约自述（字段清单、版本门槛），只读免鉴权，供前端渲染说明；
//   - POST /logs/format/preview  逐节点下发计划 + 风险告警，不写任何数据；
//   - POST /logs/format/apply    落配置版本 + 建变更单（draft），仍需人去提交/审批才真正发布。
//
// 为什么 apply 只到 draft 而不直接执行：日志格式变更要 reload nginx，属于线上动作，
// 必须走 M3 既有的提交/审批/灰度/回滚闭环，不给任何"一键直发线上"的旁路。
package handler

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/ent/schema"
	"github.com/th/ngxcp/internal/domain/logfmt"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// LogFormatOrchestrator 是本处理器需要的编排能力（由 logfmt.Service 实现）。
// 以窄接口声明，使 handler 层可在无 DB / 无节点的条件下单测。
type LogFormatOrchestrator interface {
	Plan(ctx context.Context, nodeID int, opts logfmt.SnippetOptions) (*logfmt.NodePlan, error)
	Apply(ctx context.Context, in logfmt.ApplyInput) (*logfmt.ApplyResult, error)
}

// LogFormatHandler 暴露标准日志格式的预览与下发（T060）。
type LogFormatHandler struct {
	svc LogFormatOrchestrator
}

// NewLogFormatHandler 构造处理器。
func NewLogFormatHandler(svc LogFormatOrchestrator) *LogFormatHandler {
	return &LogFormatHandler{svc: svc}
}

// logFormatReq 是预览/下发的公共请求体。
type logFormatReq struct {
	NodeIDs    []int                 `json:"node_ids"`    // 目标节点，必填
	FormatName string                `json:"format_name"` // 可选，默认 ngxcp_json
	LogPath    string                `json:"log_path"`    // 可选，默认按存量 access_log 目录推导
	Buffer     string                `json:"buffer"`      // 可选，默认 32k
	Flush      string                `json:"flush"`       // 可选，默认 5s
	Strategy   schema.DeployStrategy `json:"strategy"`    // 仅 apply 使用，缺省 serial+60s+自动回滚
	CreatedBy  string                `json:"created_by"`
	Comment    string                `json:"comment"`
}

// options 提取渲染参数（Normalize/Validate 由 domain 层统一负责，避免两处规则漂移）。
func (r logFormatReq) options() logfmt.SnippetOptions {
	return logfmt.SnippetOptions{
		FormatName: r.FormatName,
		LogPath:    r.LogPath,
		Buffer:     r.Buffer,
		Flush:      r.Flush,
	}
}

// Contract 返回下发格式的自述信息。
//
//	GET /api/v1/logs/format
//	→ { code, data:{ format_name, fields, snippet_file, min_nginx_version, log_file } }
//
// 前端据此渲染"平台会写入哪些字段"，也让排查者能直接比对节点上的实际片段。
func (h *LogFormatHandler) Contract(c *gin.Context) {
	response.OK(c, gin.H{
		"format_name":       logfmt.DefaultFormatName,
		"fields":            logfmt.FieldKeys(),
		"snippet_file":      logfmt.SnippetFileName,
		"log_file":          logfmt.DefaultLogFile,
		"min_nginx_version": logfmt.MinNginxVersion,
		"buffer":            logfmt.DefaultBuffer,
		"flush":             logfmt.DefaultFlush,
	})
}

// Preview 逐节点生成下发计划（不写库、不建变更单）。
//
//	POST /api/v1/logs/format/preview   body { node_ids:[1,2], format_name?, log_path?, buffer?, flush? }
//	→ { code, data:{ nodes:[NodePlan], blocked:[{node_id,code,message}] } }
//
// 与 apply 的差异是**故意的**：apply 遇任一节点不可行就整体失败（不允许集群内格式不一致），
// 而 preview 会把每台的失败原因都列出来，否则用户只能看到第一台的报错、逐台挤牙膏式修。
func (h *LogFormatHandler) Preview(c *gin.Context) {
	var req logFormatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体解析失败"))
		return
	}
	if len(req.NodeIDs) == 0 {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "node_ids 不能为空"))
		return
	}
	plans := make([]*logfmt.NodePlan, 0, len(req.NodeIDs))
	blocked := make([]gin.H, 0)
	for _, id := range req.NodeIDs {
		p, err := h.svc.Plan(c.Request.Context(), id, req.options())
		if err != nil {
			blocked = append(blocked, gin.H{
				"node_id": id,
				"code":    apperr.CodeOf(err),
				"message": err.Error(),
			})
			continue
		}
		plans = append(plans, p)
	}
	response.OK(c, gin.H{
		"nodes":     plans,
		"blocked":   blocked,
		"appliable": len(blocked) == 0,
	})
}

// Apply 下发标准日志格式：写配置版本 + 创建 draft 变更单。
//
//	POST /api/v1/logs/format/apply   body { node_ids:[1,2], format_name?, log_path?, strategy?, created_by?, comment? }
//	→ { code, data:{ change_order_id, status, format_name, fields, nodes:[...] } }
//
// 返回的 change_order_id 需再调 /change-orders/:id/submit 才进入发布流水线。
func (h *LogFormatHandler) Apply(c *gin.Context) {
	var req logFormatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体解析失败"))
		return
	}
	res, err := h.svc.Apply(c.Request.Context(), logfmt.ApplyInput{
		NodeIDs:   req.NodeIDs,
		Options:   req.options(),
		Strategy:  req.Strategy,
		CreatedBy: req.CreatedBy,
		Comment:   req.Comment,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, res)
}
