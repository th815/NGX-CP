// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/lvs"
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
