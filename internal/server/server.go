package server

import (
	"context"
	"log/slog"
	"net"
	"os/signal"
	"syscall"

	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/agent/transport"
	"github.com/th/ngxcp/internal/config"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/pkg/logging"
	"github.com/th/ngxcp/internal/pkg/pki"
	"github.com/th/ngxcp/internal/repo"
)

// Run 启动控制面：HTTP 服务（M1 节点域骨架 + 鉴权 + 审计）+ Agent gRPC 服务（T014 注册 / T015 心跳）。
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

	// 加载/初始化控制面 PKI（CA + 服务端证书）。缺失时自动创建，仅限开发态。
	ca, err := pki.LoadOrCreateCA(cfg.PKIDir)
	if err != nil {
		return apperr.Wrap(apperr.CodeUnavailable, "加载 PKI 失败", err)
	}

	// 共享的节点服务：HTTP 签发接入令牌与 gRPC 校验/心跳落库共用同一实例。
	nodeSvc := node.New(client)

	// T015 会话管理：会话表 + 心跳超时扫描器。
	sessions := session.NewSessionManager(slog.Default())
	hbCfg := session.HeartbeatConfig{
		Interval:      cfg.AgentHeartbeatInterval,
		Timeout:       cfg.AgentHeartbeatTimeout,
		ReconnectBase: cfg.AgentReconnectBase,
		ReconnectMax:  cfg.AgentReconnectMax,
		ClockSkewWarn: cfg.AgentClockSkewWarn,
	}

	// Agent gRPC 服务：注册（TLS+token）走 Register RPC，其余 RPC 强制 mTLS；Heartbeat/ReportCapability 落库。
	agentSrv := transport.NewServer(nil, ca, nodeSvc, nodeSvc, sessions, hbCfg)
	tlsCfg, err := ca.GRPCServerTLSConfig()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "构造 Agent gRPC TLS 配置失败", err)
	}
	grpcSrv := agentSrv.BuildGRPCServer(tlsCfg)
	agentLis, err := net.Listen("tcp", cfg.AgentGRPC)
	if err != nil {
		return apperr.Wrap(apperr.CodeUnavailable, "监听 Agent gRPC 端口失败", err)
	}
	go func() {
		logging.Ctx(nil).Info().Str("listen", cfg.AgentGRPC).Msg("ngxcp-agent gRPC ready (T014/T015)")
		if err := grpcSrv.Serve(agentLis); err != nil {
			logging.Ctx(nil).Error().Err(err).Msg("agent gRPC 异常退出")
		}
	}()

	// 心跳超时扫描器：用控制面本地时间，超时无心跳 → nodeSvc.MarkOffline（online → offline）。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go sessions.StartScanner(ctx, hbCfg.Timeout/3, hbCfg.Timeout, func(id int) {
		if err := nodeSvc.MarkOffline(context.Background(), id); err != nil {
			logging.Ctx(nil).Debug().Int("node_id", id).Err(err).Msg("mark offline skipped")
		}
	})

	// HTTP 控制面（阻塞，直到进程退出）。
	r := buildRouter(cfg, nodeSvc, sessions)
	logging.Ctx(nil).Info().Str("listen", cfg.Listen).Msg("ngxcp-server ready (M1)")
	return r.Run(cfg.Listen)
}
