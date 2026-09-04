// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package handler 实现发布引擎的 HTTP 处理器（T030：变更单模型与状态机）。
//
// 暴露变更单的生命周期端点：创建(draft) / 列表 / 详情 / 提交 / 批准 / 拒绝 / 取消。
// 执行流水线与灰度（T032/T034/T035）后续接入，本文件只负责状态机的合法流转。
package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/ent/schema"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// DeployHandler 发布引擎处理器。
type DeployHandler struct {
	svc *deploy.Service
}

// NewDeployHandler 构造发布处理器。
func NewDeployHandler(svc *deploy.Service) *DeployHandler {
	return &DeployHandler{svc: svc}
}

type createChangeOrderReq struct {
	Title             string                `json:"title"`
	Type              string                `json:"type"`
	Source            string                `json:"source"`
	TargetNodes       []int                 `json:"target_nodes"`
	ConfigRevisionIDs []int                 `json:"config_revision_ids"`
	Strategy          schema.DeployStrategy `json:"strategy"`
	CreatedBy         string                `json:"created_by"`
	Comment           string                `json:"comment"`
}

type approveReq struct {
	ApprovedBy string `json:"approved_by"`
}

// Create 创建一条 draft 状态的变更单。
//
//	POST /api/v1/change-orders   body { title, type, source?, target_nodes?, config_revision_ids?, strategy?, created_by?, comment? }
func (h *DeployHandler) Create(c *gin.Context) {
	var req createChangeOrderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体解析失败"))
		return
	}
	if req.Title == "" {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "变更单标题不能为空"))
		return
	}
	if req.Type == "" {
		req.Type = "config"
	}
	co, err := h.svc.CreateDraft(c.Request.Context(), deploy.CreateInput{
		Title:             req.Title,
		Type:              req.Type,
		Source:            req.Source,
		TargetNodes:       req.TargetNodes,
		ConfigRevisionIDs: req.ConfigRevisionIDs,
		Strategy:          req.Strategy,
		CreatedBy:         req.CreatedBy,
		Comment:           req.Comment,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, co)
}

// List 列出变更单（?status= 可选过滤）。
//
//	GET /api/v1/change-orders
func (h *DeployHandler) List(c *gin.Context) {
	items, err := h.svc.List(c.Request.Context(), c.Query("status"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, len(items))
}

// Get 取单条变更单详情。
//
//	GET /api/v1/change-orders/:id
func (h *DeployHandler) Get(c *gin.Context) {
	id, err := parseDeployID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	co, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, co)
}

// Submit 提交：按审批规则决定 draft → pending_approval 或直达 pending。
//
//	POST /api/v1/change-orders/:id/submit
//	响应含 approval_required / required_by，便于前端决定展示审批入口。
func (h *DeployHandler) Submit(c *gin.Context) {
	id, err := parseDeployID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	need, rule, err := h.svc.EvaluateApproval(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.svc.Submit(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	got, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"id":                id,
		"status":            string(got.Status),
		"approval_required": need,
		"required_by":       rule,
	})
}

// Approve 批准 pending_approval → pending，并记录审批人。
//
//	POST /api/v1/change-orders/:id/approve   body { approved_by? }
func (h *DeployHandler) Approve(c *gin.Context) {
	id, err := parseDeployID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	var req approveReq
	_ = c.ShouldBindJSON(&req)
	approver := req.ApprovedBy
	if approver == "" {
		approver = "admin"
	}
	if err := h.svc.Approve(c.Request.Context(), id, approver); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"id": id, "status": string(deploy.StatusPending), "approved_by": approver})
}

// Reject 拒绝 pending_approval → rejected。
//
//	POST /api/v1/change-orders/:id/reject
func (h *DeployHandler) Reject(c *gin.Context) {
	id, err := parseDeployID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.svc.Reject(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"id": id, "status": string(deploy.StatusRejected)})
}

// Cancel 从可取消状态 → canceled。
//
//	POST /api/v1/change-orders/:id/cancel
func (h *DeployHandler) Cancel(c *gin.Context) {
	id, err := parseDeployID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.svc.Cancel(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"id": id, "status": string(deploy.StatusCanceled)})
}

// parseDeployID 解析变更单的 :id 路径参数。
func parseDeployID(c *gin.Context) (int, error) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		return 0, apperr.New(apperr.CodeInvalid, "非法的变更单 ID")
	}
	return id, nil
}
