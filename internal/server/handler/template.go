// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
	configstore "github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// TemplateHandler 暴露配置模板与三级变量的 HTTP 端点（T027）。
type TemplateHandler struct {
	svc *configstore.TemplateService
}

// NewTemplateHandler 构造模板处理器。
func NewTemplateHandler(svc *configstore.TemplateService) *TemplateHandler {
	return &TemplateHandler{svc: svc}
}

// variableView 是变量对外展示结构；secret 值已打码。
type variableView struct {
	Scope    string `json:"scope"`
	TargetID int    `json:"target_id"`
	Key      string `json:"key"`
	Value    string `json:"value"`
	Secret   bool   `json:"secret"`
}

// ListTemplates GET /api/v1/templates
func (h *TemplateHandler) ListTemplates(c *gin.Context) {
	list, err := h.svc.ListTemplates(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, list, len(list))
}

// GetTemplate GET /api/v1/templates/:id
func (h *TemplateHandler) GetTemplate(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "模板 ID 非法"))
		return
	}
	tmpl, err := h.svc.GetTemplate(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, tmpl)
}

type renderReq struct {
	NodeIDs []int `json:"node_ids"`
}

// RenderTemplate POST /api/v1/templates/:id/render
// 按节点批量渲染模板，返回 node_id → 配置文本。
func (h *TemplateHandler) RenderTemplate(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "模板 ID 非法"))
		return
	}
	var req renderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体解析失败"))
		return
	}
	if len(req.NodeIDs) == 0 {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "node_ids 不可为空"))
		return
	}
	tmpl, err := h.svc.GetTemplate(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	out, err := h.svc.RenderForNodes(c.Request.Context(), tmpl, req.NodeIDs)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

// ListVariables GET /api/v1/variables?scope=&target_id=
// secret 变量的值已打码为 ******。
func (h *TemplateHandler) ListVariables(c *gin.Context) {
	var scope *configstore.VariableScope
	if s := c.Query("scope"); s != "" {
		sc := configstore.VariableScope(s)
		scope = &sc
	}
	var targetID *int
	if t := c.Query("target_id"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil {
			response.Fail(c, apperr.New(apperr.CodeInvalid, "target_id 非法"))
			return
		}
		targetID = &n
	}
	vars, err := h.svc.ListVariables(c.Request.Context(), scope, targetID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	views := make([]variableView, 0, len(vars))
	for _, v := range vars {
		views = append(views, variableView{
			Scope:    string(v.Scope),
			TargetID: v.TargetID,
			Key:      v.Key,
			Value:    configstore.MaskedValue(v),
			Secret:   v.Secret,
		})
	}
	response.List(c, views, len(views))
}

type setVariableReq struct {
	Scope    string `json:"scope"`
	TargetID int    `json:"target_id"`
	Key      string `json:"key"`
	Value    string `json:"value"`
	Secret   bool   `json:"secret"`
}

// SetVariable POST /api/v1/variables
func (h *TemplateHandler) SetVariable(c *gin.Context) {
	var req setVariableReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体解析失败"))
		return
	}
	scope := configstore.VariableScope(req.Scope)
	if scope != configstore.ScopeGlobal && scope != configstore.ScopeCluster && scope != configstore.ScopeNode {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "scope 必须是 global/cluster/node"))
		return
	}
	if req.Key == "" {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "key 不可为空"))
		return
	}
	if err := h.svc.SetVariable(c.Request.Context(), scope, req.TargetID, req.Key, req.Value, req.Secret); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"ok": true})
}
