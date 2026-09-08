package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/security"
	"github.com/th/ngxcp/internal/server/response"
)

// SecurityHandler 暴露安全事件 API（T067）：事件流查询 + 处置状态机。
// 只读列表/详情放开；处置（handle）是写操作，由 router 的 auth 中间件保护。
type SecurityHandler struct {
	store security.EventStore
}

// NewSecurityHandler 构造安全事件处理器。
func NewSecurityHandler(store security.EventStore) *SecurityHandler {
	return &SecurityHandler{store: store}
}

// List 处理安全事件列表。
//
//	GET /api/v1/security/events?level=CRITICAL&handled=false&page=1&size=50
//	→ { code, data:{ items:[Event], total } }
func (h *SecurityHandler) List(c *gin.Context) {
	var f security.ListFilter
	if v := c.Query("level"); v != "" {
		f.Level = v
	}
	if v := c.Query("handled"); v != "" {
		b := v == "true" || v == "1"
		f.Handled = &b
	}
	f.Page, _ = strconv.Atoi(c.Query("page"))
	f.Size, _ = strconv.Atoi(c.Query("size"))
	items, total, err := h.store.List(c.Request.Context(), f)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": items, "total": total})
}

// Get 处理单事件详情（含证据样本）。
//
//	GET /api/v1/security/events/:id
//	→ { code, data: Event }
func (h *SecurityHandler) Get(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "非法事件 ID"))
		return
	}
	ev, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, ev)
}

// secHandleReq 是 POST /security/events/:id/handle 的请求体。
type secHandleReq struct {
	Action string `json:"action"` // blocked | ignored
}

// Handle 处置事件（置 handled 锁 + 动作）。已处置返回 409 冲突（防重复动作）。
//
//	POST /api/v1/security/events/:id/handle  { "action": "blocked" }
//	→ { code, data:{ id, handled:true, action } }
func (h *SecurityHandler) Handle(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "非法事件 ID"))
		return
	}
	var req secHandleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "请求体格式错误", "detail": err.Error()})
		return
	}
	if err := h.store.Handle(c.Request.Context(), id, req.Action); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"id": id, "handled": true, "action": req.Action})
}
