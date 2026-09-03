package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/pkg/logging"
)

// RequestLogger 记录每个请求的方法、路径、状态码与耗时。
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logging.Ctx(c.Request.Context()).Info().
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Int("status", c.Writer.Status()).
			Dur("latency", time.Since(start)).
			Msg("request")
	}
}
