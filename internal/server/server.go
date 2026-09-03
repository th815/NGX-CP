package server

import (
	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/config"
	"github.com/th/ngxcp/internal/pkg/logging"
	"github.com/th/ngxcp/internal/pkg/version"
	gmw "github.com/th/ngxcp/internal/server/middleware"
)

// Run 启动控制面 HTTP 服务（M0：仅骨架路由）。
func Run(cfg *config.Config) error {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gmw.Recovery())
	r.Use(gmw.RequestLogger())

	r.GET("/health", func(c *gin.Context) { OK(c, gin.H{"status": "ok"}) })

	v1 := r.Group("/api/v1")
	{
		v1.GET("/version", func(c *gin.Context) {
			OK(c, gin.H{"version": version.String()})
		})
		// M1 起填充真实数据
		v1.GET("/nodes", func(c *gin.Context) { List(c, []any{}, 0) })
	}

	logging.Ctx(nil).Info().Str("listen", cfg.Listen).Msg("ngxcp-server ready (M0 skeleton)")
	return r.Run(cfg.Listen)
}
