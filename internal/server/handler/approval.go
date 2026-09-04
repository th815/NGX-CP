// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package handler 实现审批流相关的 HTTP 处理器（T036）。
//
// 暴露审批记录的查询端点：列表（按状态过滤）+ 按变更单查询单条。
// 审批的"动作"端点（批准/拒绝）复用 change-orders 上的 deploy 处理器，
// 以保持变更单生命周期入口的单一性。
package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/server/response"
)

// ApprovalHandler 审批流查询处理器。
type ApprovalHandler struct {
	svc *deploy.Service
}

// NewApprovalHandler 构造审批处理器。
func NewApprovalHandler(svc *deploy.Service) *ApprovalHandler {
	return &ApprovalHandler{svc: svc}
}

// List 列出审批记录（?status=pending|approved|rejected|expired 可选过滤）。
//
//	GET /api/v1/approvals
func (h *ApprovalHandler) List(c *gin.Context) {
	items, err := h.svc.ListApprovals(c.Request.Context(), c.Query("status"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, len(items))
}

// GetForOrder 取某变更单的最新审批记录（含命中规则、审批人、超时时间）。
//
//	GET /api/v1/change-orders/:id/approval
func (h *ApprovalHandler) GetForOrder(c *gin.Context) {
	id, err := parseDeployID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	a, err := h.svc.GetApproval(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, a)
}
