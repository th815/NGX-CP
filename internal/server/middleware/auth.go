// Package middleware 提供控制面 HTTP 中间件：鉴权、审计、日志、恢复。
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// Principal 是请求上下文里的已认证主体。
type Principal struct {
	Role string
}

// CtxPrincipalKey 是 gin 上下文里存放 Principal 的键。
const CtxPrincipalKey = "principal"

// unauthorized 以统一信封返回 401，避免与 server 包形成循环依赖。
func unauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"code":    int(apperr.CodeUnauthorized),
		"message": msg,
	})
}

// RequireAuth 校验 Bearer 令牌（M1 最小可用：单一管理员令牌，多角色留到 M9）。
// 若配置的 adminToken 为空，则所有写接口一律 401（fail-closed）。
func RequireAuth(adminToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if adminToken == "" {
			unauthorized(c, "写接口已禁用：未配置 auth_admin_token")
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		if token == "" || token != adminToken {
			unauthorized(c, "未授权")
			return
		}
		c.Set(CtxPrincipalKey, &Principal{Role: "admin"})
		c.Next()
	}
}
