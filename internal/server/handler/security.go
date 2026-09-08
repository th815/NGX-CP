package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/security"
	"github.com/th/ngxcp/internal/server/response"
)

// SecurityHandler 暴露安全事件 API（T067）与封禁变更单入口（T068）。
// 只读列表/详情放开；处置（handle）与封禁（block/unblock）是写操作，由 router 的 auth 中间件保护。
type SecurityHandler struct {
	store    security.EventStore
	blockSvc *security.BlockService
}

// NewSecurityHandler 构造安全事件处理器。
func NewSecurityHandler(store security.EventStore, blockSvc *security.BlockService) *SecurityHandler {
	return &SecurityHandler{store: store, blockSvc: blockSvc}
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

// blockReq 是封禁/解封请求体。
type blockReq struct {
	IP       string `json:"ip"`
	Reason   string `json:"reason"`
	Operator string `json:"operator"`
}

// operatorOrDefault 取操作人，缺省用 "admin"。
func operatorOrDefault(s string) string {
	if s == "" {
		return "admin"
	}
	return s
}

// Block 封禁一个 IP：生成 security_block 变更单并走 M3 发布流水线。
//
//	POST /api/v1/security/blocklist  { "ip":"1.2.3.4", "reason":"...", "operator":"admin" }
//	→ { code, data: ChangeOrder }
func (h *SecurityHandler) Block(c *gin.Context) {
	var req blockReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体格式错误"))
		return
	}
	if req.IP == "" {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "ip 不能为空"))
		return
	}
	co, err := h.blockSvc.BlockIP(c.Request.Context(), req.IP, req.Reason, operatorOrDefault(req.Operator), false)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, co)
}

// Unblock 解封一个 IP：同样走变更单（覆盖原封禁文件，移除 deny）。
//
//	DELETE /api/v1/security/blocklist/:ip  { "reason":"误报", "operator":"admin" }
//	→ { code, data: ChangeOrder }
func (h *SecurityHandler) Unblock(c *gin.Context) {
	ip := c.Param("ip")
	if ip == "" {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "ip 不能为空"))
		return
	}
	var req blockReq
	_ = c.ShouldBindJSON(&req) // 可选体：reason / operator
	co, err := h.blockSvc.UnblockIP(c.Request.Context(), ip, req.Reason, operatorOrDefault(req.Operator))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, co)
}

// BlockEvent 从安全事件一键封禁：提取样本中的来源 IP → 生成 security_block 变更单。
//
//	POST /api/v1/security/events/:id/block  { "operator":"admin" }
//	→ { code, data: ChangeOrder }
func (h *SecurityHandler) BlockEvent(c *gin.Context) {
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
	var req blockReq
	_ = c.ShouldBindJSON(&req)
	co, err := h.blockSvc.BlockEvent(c.Request.Context(), ev, operatorOrDefault(req.Operator), false)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, co)
}

// Rules 返回当前生效的攻击检测规则集（T066 DefaultRules）。只读展示用，
// 阈值/动作编辑持久化属后续里程碑（见 docs/tasks/M6-logs-security.md T070）。
//
//	GET /api/v1/security/rules
//	→ { code, data:[Rule] }
func (h *SecurityHandler) Rules(c *gin.Context) {
	response.OK(c, security.DefaultRules())
}
