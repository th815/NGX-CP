//go:build !webui

package web

import "github.com/gin-gonic/gin"

// RegisterWebUI 非 webui 构建下为空实现（开发态不内嵌前端，由 Vite 代理 / 独立托管）。
func RegisterWebUI(r *gin.Engine) {}
