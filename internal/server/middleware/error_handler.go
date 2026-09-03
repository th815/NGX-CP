// Package middleware 提供 gin 中间件（恢复、请求日志）。
package middleware

import (
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/pkg/logging"
)

// Recovery 捕获 panic，记录堆栈并返回 500，绝不向客户端泄露内部细节。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logging.Ctx(c.Request.Context()).Error().
					Interface("panic", r).Str("stack", string(debug.Stack())).Msg("panic recovered")
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code":    int(apperr.CodeInternal),
					"message": "服务器内部错误",
				})
			}
		}()
		c.Next()
	}
}
