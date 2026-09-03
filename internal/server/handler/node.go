// Package handler 实现控制面 HTTP 处理器（M1 节点域）。
package handler

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// NodeHandler 节点相关处理器。
type NodeHandler struct {
	svc  *node.Service
	// skew 返回节点与控制面的时钟偏差（秒）及是否在线记录到偏差；nil 表示无会话管理。
	skew func(int) (float64, bool)
}

// NewNodeHandler 构造节点处理器。skew 可选（无会话管理时传 nil）。
func NewNodeHandler(svc *node.Service, skew func(int) (float64, bool)) *NodeHandler {
	return &NodeHandler{svc: svc, skew: skew}
}

// List 列出节点（支持 role / status 过滤与分页）。
func (h *NodeHandler) List(c *gin.Context) {
	o := node.ListOpts{
		Role:   c.Query("role"),
		Status: c.Query("status"),
	}
	o.Limit, _ = strconv.Atoi(c.DefaultQuery("limit", "50"))
	o.Offset, _ = strconv.Atoi(c.DefaultQuery("offset", "0"))
	items, total, err := h.svc.List(c.Request.Context(), o)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, items, total)
}

// Get 获取单个节点。
func (h *NodeHandler) Get(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	out, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 注入实时会话指标：时钟偏差（T015）。
	if h.skew != nil {
		if s, ok := h.skew(id); ok {
			out.ClockSkewSeconds = &s
		}
	}
	response.OK(c, out)
}

// Create 新建节点（需鉴权）。
func (h *NodeHandler) Create(c *gin.Context) {
	var in node.CreateNodeIn
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInvalid, "请求体格式错误", err))
		return
	}
	out, err := h.svc.Create(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

// Update 局部更新节点（需鉴权）。
func (h *NodeHandler) Update(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in node.UpdateNodeIn
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInvalid, "请求体格式错误", err))
		return
	}
	out, err := h.svc.Update(c.Request.Context(), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

// Delete 删除节点（需鉴权）。
func (h *NodeHandler) Delete(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": id})
}

// IssueEnrollToken 为指定节点生成一次性接入令牌（需鉴权）。
func (h *NodeHandler) IssueEnrollToken(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	ttl := time.Hour
	if q := c.Query("ttl"); q != "" {
		if d, err := time.ParseDuration(q); err == nil {
			ttl = d
		}
	}
	tok, exp, err := h.svc.IssueEnrollToken(c.Request.Context(), id, ttl)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"token": tok, "node_id": id, "expires_at": exp})
}

// GetCapability 查看节点能力基线占位。
func (h *NodeHandler) GetCapability(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	cap, err := h.svc.GetCapability(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, cap)
}

// RefreshCapability 触发一次能力刷新（需鉴权）。
func (h *NodeHandler) RefreshCapability(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if err := h.svc.RefreshCapability(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"node_id": id, "triggered": true})
}

func parseID(c *gin.Context) (int, error) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		return 0, apperr.New(apperr.CodeInvalid, "非法的节点 ID")
	}
	return id, nil
}
