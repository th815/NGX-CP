// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package handler 实现控制面 HTTP 处理器。本文件落地 T026 漂移检测的 HTTP 入口。
package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// DriftHandler 处理配置漂移检测查询与手动触发。
type DriftHandler struct {
	detector config.DriftChecker
}

// NewDriftHandler 构造漂移处理器。
func NewDriftHandler(d config.DriftChecker) *DriftHandler {
	return &DriftHandler{detector: d}
}

// ListDrift 返回漂移检测报告。
//
//	GET /api/v1/configs/drift?node_id=1   → 该节点最近一次报告
//	GET /api/v1/configs/drift             → 所有已缓存节点报告
//
// node_id 非法 → 4001；检测器未初始化 → 4103。无缓存报告时返回空 items（不报错）。
func (h *DriftHandler) ListDrift(c *gin.Context) {
	if h.detector == nil {
		response.Fail(c, apperr.New(apperr.CodeUnavailable, "漂移检测器未初始化"))
		return
	}
	if s := c.Query("node_id"); s != "" {
		id, err := strconv.Atoi(s)
		if err != nil || id <= 0 {
			response.Fail(c, apperr.New(apperr.CodeInvalid, "非法 node_id"))
			return
		}
		r, ok := h.detector.GetReport(id)
		if !ok {
			response.OK(c, gin.H{
				"node_id": id,
				"items":   []any{},
				"note":    "尚未产生漂移报告（等待一次配置树上报或 POST /configs/drift 手动触发）",
			})
			return
		}
		response.OK(c, r)
		return
	}
	response.List(c, h.detector.Reports(), len(h.detector.Reports()))
}

// CheckDrift 手动提交节点实际配置，立即触发一次漂移检测（用于无 Agent 时的按需校验/测试）。
//
//	POST /api/v1/configs/drift   body { "node_id": 1, "files": [{"path":"...","sha":"...","content":"..."}] }
func (h *DriftHandler) CheckDrift(c *gin.Context) {
	if h.detector == nil {
		response.Fail(c, apperr.New(apperr.CodeUnavailable, "漂移检测器未初始化"))
		return
	}
	var in struct {
		NodeID int                         `json:"node_id"`
		Files  []config.ReportedConfigFile `json:"files"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体格式非法").WithDetail(err.Error()))
		return
	}
	if in.NodeID <= 0 {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "node_id 必填"))
		return
	}
	report, err := h.detector.RecordActual(c.Request.Context(), in.NodeID, in.Files)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"node_id":  in.NodeID,
		"items":    report.Items,
		"count":    len(report.Items),
		"critical": report.CriticalCount(),
	})
}
