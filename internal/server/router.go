package server

import (
	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/config"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/server/handler"
	"github.com/th/ngxcp/internal/server/middleware"
	"github.com/th/ngxcp/internal/server/response"
	"github.com/th/ngxcp/internal/pkg/version"
)

// buildRouter 构建 gin 引擎：中间件 + 路由（M1 节点域）。
// 鉴权策略（M1 最小可用）：只读接口放开，写接口与接入令牌需 Bearer 令牌。
// nodeSvc 必须与 Agent gRPC 服务共用同一实例——接入令牌的内存表在两处共享（HTTP 签发 / gRPC 校验）。
// sessions 提供实时会话指标（时钟偏差），注入 handler 后随节点详情返回。
func buildRouter(cfg *config.Config, nodeSvc *node.Service, sessions *session.SessionManager) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestLogger())
	r.Use(middleware.Audit(nodeSvc.Client()))

	r.GET("/health", func(c *gin.Context) {
		response.OK(c, gin.H{"status": "ok"})
	})

	v1 := r.Group("/api/v1")
	{
		v1.GET("/version", func(c *gin.Context) {
			response.OK(c, gin.H{"version": version.String()})
		})

		nh := handler.NewNodeHandler(nodeSvc, sessions.ClockSkewSeconds)
		ns := v1.Group("/nodes")
		{
			// 只读
			ns.GET("", nh.List)
			ns.GET("/:id", nh.Get)
			ns.GET("/:id/capability", nh.GetCapability)

			// 写操作 + 接入令牌：需鉴权
			auth := middleware.RequireAuth(cfg.AuthAdminToken)
			ns.POST("", auth, nh.Create)
			ns.POST("/:id/enroll-token", auth, nh.IssueEnrollToken)
			ns.PATCH("/:id", auth, nh.Update)
			ns.DELETE("/:id", auth, nh.Delete)
			ns.POST("/:id/refresh", auth, nh.RefreshCapability)
		}
	}
	return r
}
