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
