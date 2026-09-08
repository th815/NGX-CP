package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/agent/transport"
	"github.com/th/ngxcp/internal/config"
	"github.com/th/ngxcp/internal/crypto"
	certdom "github.com/th/ngxcp/internal/domain/cert"
	configstore "github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/domain/config/rules"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/logstore"
	"github.com/th/ngxcp/internal/lvs"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/pkg/logging"
	"github.com/th/ngxcp/internal/pkg/pki"
	"github.com/th/ngxcp/internal/repo"
	"github.com/th/ngxcp/internal/security"
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

	// 证书信封加密主密钥：缺失时证书上传/解密不可用，但其余功能照常。
	kms, kmsErr := crypto.NewKMSFromEnv()
	if kmsErr != nil {
		logging.Ctx(nil).Warn().Err(kmsErr).Msg("未配置主密钥，证书上传/解密暂不可用（设置 NGXCP_MASTER_KEY 或写入 /etc/ngxcp/master.key）")
	}
	certSvc := certdom.New(client, kms)

	// M5 LVS 拓扑/渲染/门禁服务（复用同一 ent 客户端）。
	lvsSvc := lvs.New(client)

	// T063：日志检索依赖的落库后端。生产经 NGXCP_LOGSTORE_DSN 连 ClickHouse
	// （建表 + 限内存 6G）；未配置时回落 MemStorage（检索可用但无真实数据，
	// 待 Agent→控制面日志传输通道接通后才有内容）。绝不因缺 DSN 而阻断启动。
	var logStore logstore.Storage
	var ch *logstore.ClickHouseStorage
	if dsn := os.Getenv("NGXCP_LOGSTORE_DSN"); dsn != "" {
		memBytes, memErr := logstore.ParseMemLimit("6G")
		if memErr != nil {
			memBytes = 6 * 1024 * 1024 * 1024 // 解析失败回落 6G
		}
		c, chErr := logstore.NewClickHouse(dsn, memBytes, 7)
		if chErr != nil {
			logging.Ctx(nil).Warn().Err(chErr).Msg("ClickHouse 连接失败，日志检索回落内存存储")
			logStore = logstore.NewMemStorage()
		} else {
			ch = c
			if schemaErr := ch.ApplySchema(context.Background()); schemaErr != nil {
				logging.Ctx(nil).Warn().Err(schemaErr).Msg("ClickHouse 建表失败，日志检索回落内存存储")
				ch = nil
				logStore = logstore.NewMemStorage()
			} else {
				logStore = ch
			}
		}
	} else {
		logStore = logstore.NewMemStorage()
	}

	// T063 补：日志传输接线——Agent 经心跳 LOG_BATCH 上报 → Ingester 攒批 → logStore 入库。
	logIngester := logstore.NewIngester(logStore)

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
	// T063 补：把日志批次接收器注入 gRPC 服务端，使 LOG_BATCH 上报被消费。
	agentSrv.SetLogIngester(logIngester)

	// M5 T054/T055：注入远程权重执行器（经 Agent SET_RS_WEIGHT 通道）与发布前门禁。
	lvsSvc.SetWeightSetter(lvs.NewRemoteSetter(agentSrv, lvs.ResolveDirectorNode(client)))
	lvsSvc.SetGate(lvs.NewGate(lvs.NodeStatusCompliance(client)))
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

	// T063 补：启动日志批次攒批 flush 周期 goroutine（与进程同生命周期）。
	go logIngester.Start(ctx)
	// T056：启动脑裂周期监测（检测到双 Director 同时持 VIP 时打 CRITICAL 日志/告警），与进程同生命周期。
	go lvsSvc.StartSplitBrainWatch(ctx, time.Minute, nil)

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

	// T039 发布执行闭环：启动 worker 轮询 pending 单并驱动状态机收敛。
	// AgentRunner 经 Agent gRPC 心跳流把变更单推到目标节点执行，订单收敛到 success/failed，
	// 不再停在「等待执行器接入」。ctx 取消即退出（与进程同生命周期）。
	lockCfg := deploy.DefaultLockConfig()
	locks := deploy.NewLockManager(client, lockCfg)
	queue := deploy.NewQueue(deploySvc, locks, lockCfg)
	agentRunner := NewAgentRunner(deploySvc, cfgStore, agentSrv)
	deployWorker := deploy.NewWorker(queue, deploySvc, agentRunner, lockCfg)
	go deployWorker.Start(ctx)

	// T045 自动续期调度：注入 Agent 下发通道使续期后可走 T044 原子流水线重分发；
	// 仅当主密钥已配置（证书信封加密可用）时启动，否则续期无法解密凭据。
	// 调度器每日对齐 03:00 触发：到期前 30 天起自动续期；续期结果落 source=auto_renew /
	// type=cert_renew 变更单作审计；连续失败达阈值触发 CRITICAL 告警。
	certSvc.SetDeployer(agentSrv)
	if kms != nil {
		renewRecorder := &renewRecorder{deploy: deploySvc, certSvc: certSvc}
		scheduler := certdom.NewScheduler(certSvc, &certDueLister{svc: certSvc}, renewRecorder, 30*24*time.Hour,
			func(certID, fails int) {
				logging.Ctx(nil).Error().
					Int("cert_id", certID).
					Int("fail_streak", fails).
					Msg("证书自动续期连续失败达到阈值，需人工介入检查 DNS/ACME/节点下发通道")
			})
		go scheduler.Start(ctx, 24*time.Hour)
	} else {
		logging.Ctx(nil).Warn().Msg("主密钥未配置，自动续期调度未启动（设置 NGXCP_MASTER_KEY 后重启以启用）")
	}

	// T026 漂移定时巡检：ctx 取消即退出（与进程同生命周期）。
	go func() {
		if err := driftDetector.RunWorker(ctx, driftCfg.CheckInterval); err != nil && ctx.Err() == nil {
			logging.Ctx(nil).Error().Err(err).Msg("drift worker exited unexpectedly")
		}
	}()

	// T067 告警中心：把 T066 规则引擎接调度，命中 → 建 SecurityEvent（PG）+ 落库 security_alerts（CH）。
	// 无 ClickHouse（未配 NGXCP_LOGSTORE_DSN）时 backend=NoopBackend（恒返回 0，不误报），
	// 不阻断启动；配了 CH 才真正跑检测与落库。
	secSvc := security.NewEntEventStore(client)
	var secBackend security.Backend = security.NewNoopBackend()
	var secAlerts security.AlertStore
	if ch != nil {
		secBackend = ch // *ClickHouseStorage 满足 security.Backend（新增 QueryCount 方法）
		secAlerts = security.NewCHAlertStore(ch)
	}
	secEngine := security.NewEngine(secBackend)
	secSched := security.NewScheduler(secEngine, secSvc, secAlerts, security.DefaultRules())
	go secSched.Start(ctx, 30*time.Second)

	// T068 封禁变更单：把「封禁/解封 IP」转换为走 M3 流水线的 security_block 变更单。
	blockSvc := security.NewBlockService(client, deploySvc)

	// HTTP 控制面（阻塞，直到进程退出）。
	// agentSrv 同时作为 T024 校验触发入口（实现 handler.ConfigValidator），经心跳命令流驱动 Agent 跑 nginx -t。
	r := buildRouter(cfg, ca, nodeSvc, cfgStore, sessions, agentSrv, semantic, driftDetector, tmplSvc, deploySvc, hub, certSvc, lvsSvc, logStore, secSvc, blockSvc)

	// 首跑提示：尚未完成首次设置时，告知可从 Web 免 SSH 获取令牌（消除 grep config.yaml 痛点）。
	if cfg.AuthAdminToken != "" && cfg.AuthAdminTokenAckFile != "" {
		if _, statErr := os.Stat(cfg.AuthAdminTokenAckFile); statErr != nil {
			logging.Ctx(nil).Warn().
				Msg("首跑未确认：打开 Web 控制台 →「系统设置」可一键获取管理员令牌（或 GET /api/v1/admin/setup-token）；获取后点「完成首次设置」即锁定，无需 SSH")
		}
	}

	logging.Ctx(nil).Info().Str("listen", cfg.Listen).Msg("ngxcp-server ready (M1)")
	return r.Run(cfg.Listen)
}

// renewRecorder 把每次续期结果记录为一条 cert_renew 变更单（审计），并驱动到终态。
// 自动续期为系统行为，故不走人工审批：draft → pending → running → success/failed。
type renewRecorder struct {
	deploy  *deploy.Service
	certSvc *certdom.Service
}

// certDueLister 适配 cert.Service.ListDueForRenewal 到调度器期望的 DueLister.ListDue。
type certDueLister struct {
	svc *certdom.Service
}

func (l *certDueLister) ListDue(ctx context.Context, horizon time.Duration) ([]int, error) {
	return l.svc.ListDueForRenewal(ctx, horizon)
}

// RecordRenewal 实现 cert.Recorder：落一条审计变更单并收敛到终态。
func (r *renewRecorder) RecordRenewal(ctx context.Context, certID int, ok bool, detail string) error {
	title := fmt.Sprintf("自动续期证书 #%d", certID)
	if v, err := r.certSvc.Get(ctx, certID); err == nil && v != nil {
		title = fmt.Sprintf("自动续期证书 %s (#%d)", v.Domain, certID)
	}
	co, err := r.deploy.CreateDraft(ctx, deploy.CreateInput{
		Title:   title,
		Type:    "cert_renew",
		Source:  "auto_renew",
		Comment: detail,
	})
	if err != nil {
		return err
	}
	// 驱动到终态（不走审批）：pending → running → success/failed。
	if err := r.deploy.Transition(ctx, co.ID, "draft", "pending"); err != nil {
		return err
	}
	if err := r.deploy.Start(ctx, co.ID); err != nil {
		return err
	}
	if err := r.deploy.Complete(ctx, co.ID, ok, detail); err != nil {
		return err
	}
	return nil
}
