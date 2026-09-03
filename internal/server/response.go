// Package server 是控制面 HTTP 层（gin）。M0 仅暴露 /health 与 /version。
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

const maxDetail = 2000

type envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	Data    any    `json:"data,omitempty"`
}

type listEnvelope struct {
	Code    int  `json:"code"`
	Message string `json:"message"`
	Data    any `json:"data"`
	Total   int `json:"total"`
}

// OK 成功响应。
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, envelope{Code: 0, Message: "ok", Data: data})
}

// List 列表响应。
func List(c *gin.Context, items any, total int) {
	c.JSON(http.StatusOK, listEnvelope{Code: 0, Message: "ok", Data: items, Total: total})
}

// Fail 从任意 error 提取业务码并响应；Detail 超长会被截断，避免泄露大段命令输出。
func Fail(c *gin.Context, err error) {
	e := apperr.From(err)
	detail := e.Detail
	if len(detail) > maxDetail {
		detail = detail[:maxDetail] + "...(truncated)"
	}
	c.JSON(httpStatusOf(e.Code), envelope{Code: int(e.Code), Message: e.Message, Detail: detail})
}

func httpStatusOf(code apperr.Code) int {
	switch code {
	case apperr.CodeInvalid:
		return http.StatusBadRequest
	case apperr.CodeUnauthorized:
		return http.StatusUnauthorized
	case apperr.CodeForbidden:
		return http.StatusForbidden
	case apperr.CodeNotFound:
		return http.StatusNotFound
	case apperr.CodeConflict:
		return http.StatusConflict
	case apperr.CodePrecondition:
		return http.StatusPreconditionFailed
	case apperr.CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
