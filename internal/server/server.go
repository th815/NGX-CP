package server

import (
	"context"

	"github.com/th/ngxcp/internal/config"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/pkg/logging"
	"github.com/th/ngxcp/internal/repo"
)

// Run 启动控制面 HTTP 服务（M1：节点域骨架 + 鉴权 + 审计）。
// 打开数据库；若 db_auto_migrate=true 则自动建表（生产请置 false 改用 make migrate-dev）。
func Run(cfg *config.Config) error {
	client, err := repo.Open(cfg.DBDriver, cfg.DBDsn)
	if err != nil {
		return apperr.Wrap(apperr.CodeUnavailable, "打开数据库失败", err)
	}
	defer client.Close()

	if cfg.DBAutoMigrate {
		if err := client.Schema.Create(context.Background()); err != nil {
			return apperr.Wrap(apperr.CodeUnavailable, "自动建表失败", err)
		}
	}

	_ = logging.Init(cfg.LogLevel, cfg.LogPretty)
	r := buildRouter(cfg, client)
	logging.Ctx(nil).Info().Str("listen", cfg.Listen).Msg("ngxcp-server ready (M1)")
	return r.Run(cfg.Listen)
}
