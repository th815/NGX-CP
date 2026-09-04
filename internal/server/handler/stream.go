// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package handler 实现发布进度 SSE 实时推送（T037）。
//
// Stream 处理器依赖 DeployEventSource 接口（由 server.Hub 实现），
// 不直接 import server 包，避免 server→handler 的循环依赖。
package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/server/response"
)

// DeployEventSource 是 SSE 处理器订阅发布事件的接口（server.Hub 实现）。
// 用接口而非具体类型，解耦 handler 与 server 包，规避循环依赖。
type DeployEventSource interface {
	Subscribe(orderID int) (subID int, ch <-chan deploy.DeployEvent)
	Unsubscribe(subID int)
}

// StreamHandler 通过 SSE 实时推送某变更单的发布事件。
type StreamHandler struct {
	src DeployEventSource
}

// NewStreamHandler 构造 SSE 处理器。
func NewStreamHandler(src DeployEventSource) *StreamHandler {
	return &StreamHandler{src: src}
}

// Stream GET /api/v1/change-orders/:id/stream
//
// 事件流形如：
//
//	id: 12
//	data: {"order_id":17,"step":"validate","status":"running",...}
//
// 客户端可用 Last-Event-ID 在断连后请求重放（Hub 回放有界历史）。
func (h *StreamHandler) Stream(c *gin.Context) {
	id, err := parseDeployID(c)
	if err != nil {
		response.Fail(c, err)
		return
	}
	subID, ch := h.src.Subscribe(id)
	defer h.src.Unsubscribe(subID)

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no") // 关闭反代缓冲（nginx 前置场景）
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	// 心跳保活，避免中间代理静默断开长连接。
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case evt := <-ch:
			msg, e := json.Marshal(evt)
			if e != nil {
				continue
			}
			fmt.Fprintf(c.Writer, "id: %d\ndata: %s\n\n", evt.ID, msg)
			c.Writer.Flush()
		case <-ticker.C:
			fmt.Fprintf(c.Writer, ": ping\n\n")
			c.Writer.Flush()
		case <-c.Request.Context().Done():
			return
		}
	}
}
