// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package handler 实现控制面 HTTP 处理器（T021 配置中心）。
package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/server/response"
)

// ConfigHandler 配置中心处理器：配置树版本化浏览（T021）。
type ConfigHandler struct {
	store *config.ConfigStore
}

// NewConfigHandler 构造配置处理器。
func NewConfigHandler(store *config.ConfigStore) *ConfigHandler {
	return &ConfigHandler{store: store}
}

// ListByNode 按节点列出配置文件（含当前版本摘要）。?node_id=1 必填。
func (h *ConfigHandler) ListByNode(c *gin.Context) {
	nodeID, err := strconv.Atoi(c.Query("node_id"))
	if err != nil || nodeID <= 0 {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "缺少或非法的 node_id"))
		return
	}
	files, err := h.store.ListFiles(c.Request.Context(), nodeID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, files, len(files))
}

// GetFile 取单个配置文件（含当前版本内容）。
func (h *ConfigHandler) GetFile(c *gin.Context) {
	id, err := parseConfigID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	f, err := h.store.GetFile(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, f)
}

// ListRevisions 列出某配置文件的版本链（按时间倒序）。
func (h *ConfigHandler) ListRevisions(c *gin.Context) {
	id, err := parseConfigID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	revs, err := h.store.ListRevisions(c.Request.Context(), id, 0)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.List(c, revs, len(revs))
}

// parseConfigID 解析配置文件的 :id 路径参数。
func parseConfigID(c *gin.Context) (int, error) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		return 0, apperr.New(apperr.CodeInvalid, "非法的配置文件 ID")
	}
	return id, nil
}

// ManualEdit 把前端编辑器内容存为新版本（来源 manual_edit，T028 编辑→保存）。
//
//	POST /api/v1/configs/:id/manual-edit   body { "content": "...", "message": "...", "author": "..." }
//
// 成功：200 {code:0, data: 新版本视图}；内容为空：4001。
// 写入后该文件 current_revision 指向新版本，且 manual_edit 作为平台期望基线，
// 不会反过来被 T026 漂移检测判为漂移。
func (h *ConfigHandler) ManualEdit(c *gin.Context) {
	fileID, err := parseConfigID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	var in struct {
		Content string `json:"content"`
		Message string `json:"message"`
		Author  string `json:"author"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "请求体格式非法").WithDetail(err.Error()))
		return
	}
	if in.Content == "" {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "content 不可为空"))
		return
	}
	author := in.Author
	if author == "" {
		author = "web"
	}
	rev, err := h.store.CreateRevision(c.Request.Context(), fileID, []byte(in.Content), config.RevisionOpts{
		Source:  config.SourceManualEdit,
		Author:  author,
		Message: in.Message,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, rev)
}

// Diff 对任意两版配置做语义 diff（T022 + T028）。
//
//	GET /api/v1/configs/:id/diff?from=<revID>&to=<revID>
//
// 成功：200 {code:0, data:{from, to, stats:{added,deleted,changed}, hunks:[...]}}
// 参数缺省/非法：4001；版本不存在：4040。
func (h *ConfigHandler) Diff(c *gin.Context) {
	fileID, err := parseConfigID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	from, err := strconv.Atoi(c.Query("from"))
	if err != nil || from <= 0 {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "from 参数必填且为合法版本 ID"))
		return
	}
	to, err := strconv.Atoi(c.Query("to"))
	if err != nil || to <= 0 {
		response.Fail(c, apperr.New(apperr.CodeInvalid, "to 参数必填且为合法版本 ID"))
		return
	}
	res, err := h.store.DiffRevisions(c.Request.Context(), fileID, from, to)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"from":  res.OldRev,
		"to":    res.NewRev,
		"stats": res.Stats,
		"hunks": res.Hunks,
	})
}
