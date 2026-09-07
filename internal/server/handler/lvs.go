// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/lvs"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// LVSHandler 暴露 LVS / Keepalived 拓扑与虚拟服务（M5 T053，只读聚合）。
type LVSHandler struct {
	svc *lvs.Service
}

// NewLVSHandler 构造 LVS 处理器。
func NewLVSHandler(svc *lvs.Service) *LVSHandler {
	return &LVSHandler{svc: svc}
}

// Topology 返回 LVS 拓扑（Client → VIP → Director×2 → RS×N）。
// GET /api/v1/lvs/topology
func (h *LVSHandler) Topology(c *gin.Context) {
	topo, err := h.svc.Topology(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, topo)
}

// VirtualServices 返回全部虚拟服务（含所属 Director 状态）。
// GET /api/v1/lvs/virtual-services
func (h *LVSHandler) VirtualServices(c *gin.Context) {
	vs, err := h.svc.VirtualServices(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, vs)
}

// Drain 摘除 RS（权重置 0，Enabled=false）。写操作，受 T055 门禁约束。
// POST /api/v1/lvs/real-servers/:id/drain
func (h *LVSHandler) Drain(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.svc.Drain(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"id": id, "action": "drain"})
}

// Restore 恢复 RS（下发基线权重，Enabled=true）。写操作，受 T055 门禁约束。
// POST /api/v1/lvs/real-servers/:id/restore
func (h *LVSHandler) Restore(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.svc.Restore(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"id": id, "action": "restore"})
}

type rsWeightBody struct {
	Weight int `json:"weight"`
}

// SetWeight 设置 RS 基线权重并下发（权重>0 启用，=0 等效摘除）。写操作，受 T055 门禁约束。
// POST /api/v1/lvs/real-servers/:id/weight
func (h *LVSHandler) SetWeight(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	var b rsWeightBody
	if err := c.ShouldBindJSON(&b); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体解析失败："+err.Error()))
		return
	}
	if err := h.svc.SetBaselineWeight(c.Request.Context(), id, b.Weight); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"id": id, "weight": b.Weight})
}
