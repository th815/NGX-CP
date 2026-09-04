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
	configstore "github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/domain/config/rules"
	"github.com/th/ngxcp/internal/domain/deploy"
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
	// T021 配置版本化存储复用同一 ent 客户端，注入节点服务以在 SaveConfigTree 时同步版本链。
	cfgStore := configstore.New(client)
	nodeSvc := node.New(client, cfgStore)

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

	// T025 语义校验器：复用 cfgStore + ent 客户端，对节点当前配置跑规则引擎。
	// rules.yaml 缺失时自动回退到内建默认规则，保证控制面始终可用。
	rulesCfg, _ := rules.LoadConfig("configs/rules.yaml")
	semantic := configstore.NewSemanticChecker(client, cfgStore, rulesCfg)

	// T026 漂移检测器：复用 cfgStore + ent 客户端；在 SaveConfigTree 配置树上报时即时检测，
	// 并由 worker 定时巡检（默认 5 分钟）。severity 规则由 App 配置映射而来。
	severityRules := make([]configstore.SeverityRule, 0, len(cfg.DriftSeverityRules))
	for _, r := range cfg.DriftSeverityRules {
		severityRules = append(severityRules, configstore.SeverityRule{
			PathPattern: r.PathPattern,
			Severity:    r.Severity,
		})
	}
	driftCfg := configstore.DriftConfig{
		CheckInterval: cfg.DriftCheckInterval,
		AutoAlert:     cfg.DriftAutoAlert,
		AutoRemediate: cfg.DriftAutoRemediate,
		SeverityRules: severityRules,
	}
	driftDetector := configstore.NewDriftDetector(client, cfgStore, driftCfg)
	nodeSvc.SetDriftDetector(driftDetector)

	// T027 模板与三级变量服务：复用 ent 客户端，提供配置模板渲染与变量解析。
	tmplSvc := configstore.NewTemplateService(client)

	// T030 发布引擎：变更单状态机与持久化（复用同一 ent 客户端）。
	deploySvc := deploy.New(client)

	// T037 发布进度实时推送：Hub 作为事件出口注入域层，并供 SSE 处理器订阅。
	hub := NewHub(256)
	deploySvc.SetEventSink(hub)

	// T026 漂移定时巡检：ctx 取消即退出（与进程同生命周期）。
	go func() {
		if err := driftDetector.RunWorker(ctx, driftCfg.CheckInterval); err != nil && ctx.Err() == nil {
			logging.Ctx(nil).Error().Err(err).Msg("drift worker exited unexpectedly")
		}
	}()

	// HTTP 控制面（阻塞，直到进程退出）。
	// agentSrv 同时作为 T024 校验触发入口（实现 handler.ConfigValidator），经心跳命令流驱动 Agent 跑 nginx -t。
	r := buildRouter(cfg, nodeSvc, cfgStore, sessions, agentSrv, semantic, driftDetector, tmplSvc, deploySvc, hub)
	logging.Ctx(nil).Info().Str("listen", cfg.Listen).Msg("ngxcp-server ready (M1)")
	return r.Run(cfg.Listen)
}
