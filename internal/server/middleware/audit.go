package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/auditlog"
)

// Audit 是审计中间件：仅记录「成功的写操作」（失败的写操作只进日志，避免审计表被刷屏）。
// best-effort：写审计失败不影响主流程。
func Audit(client *ent.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if c.Request.Method == http.MethodGet {
			return
		}
		if c.Writer.Status() >= 400 {
			return
		}
		role := "anonymous"
		if p, ok := c.Get(CtxPrincipalKey); ok {
			if pr, ok := p.(*Principal); ok {
				role = pr.Role
			}
		}
		method := c.Request.Method
		// 后台落库，不阻塞响应
		go func() {
			_ = client.AuditLog.Create().
				SetActor(role).
				SetAction(actionFor(method)).
				SetTargetType("api").
				SetDetail(strings.ToLower(method) + " " + c.Request.URL.Path).
				SetIP(c.ClientIP()).
				Exec(context.Background())
		}()
	}
}

// actionFor 把 HTTP 写方法映射到受 validator 约束的审计枚举。
// 通用中间件无法感知具体业务实体，故以方法为单位落到合法的域枚举上；
// 后续可改为由 handler 显式传递 action 以提升粒度。
func actionFor(method string) auditlog.Action {
	switch method {
	case http.MethodPost:
		return auditlog.ActionNodeCreate
	case http.MethodPatch, http.MethodPut:
		return auditlog.ActionNodeUpdate
	case http.MethodDelete:
		return auditlog.ActionNodeDelete
	default:
		return auditlog.ActionNodeUpdate
	}
}
