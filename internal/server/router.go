package server

import (
	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/agent/session"
	"github.com/th/ngxcp/internal/agent/transport"
	"github.com/th/ngxcp/internal/config"
	configstore "github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/domain/cert"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/lvs"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/logstore"
	"github.com/th/ngxcp/internal/pkg/pki"
	"github.com/th/ngxcp/internal/pkg/version"
	"github.com/th/ngxcp/internal/server/handler"
	"github.com/th/ngxcp/internal/server/middleware"
	"github.com/th/ngxcp/internal/server/response"
	"github.com/th/ngxcp/web"
)

// buildRouter 构建 gin 引擎：中间件 + 路由（M1 节点域 + T021/T024 配置中心）。
// 鉴权策略（M1 最小可用）：只读接口放开，写接口与接入令牌需 Bearer 令牌。
// nodeSvc 必须与 Agent gRPC 服务共用同一实例——接入令牌（join_tokens / enroll_tokens 表）的
// 签发（HTTP）与校验（gRPC 注册）走同一 ent 客户端与库。
// sessions 提供实时会话指标（时钟偏差），注入 handler 后随节点详情返回。
// cfgStore 为 T021 配置版本化存储（与 nodeSvc 共用同一 ent 客户端）。
// validator 为 T024 校验触发入口（*transport.Server 经心跳命令流驱动 Agent 跑 nginx -t）。
// semantic 为 T025 语义校验器（复用 cfgStore + ent 客户端，对节点当前配置跑规则引擎）。
// drift 为 T026 漂移检测器（复用 cfgStore + ent 客户端，在配置树上报时即时检测 + 定时巡检）。
// tmplSvc 为 T027 模板与三级变量服务（复用 ent 客户端，提供配置模板渲染与变量解析）。
// agentSrv 为 *transport.Server：既作为 T024 校验触发入口（实现 handler.ConfigValidator，
// 经心跳命令流驱动 Agent 跑 nginx -t），又作为 T044 证书分发下发通道（实现 cert.Deployer）。
func buildRouter(cfg *config.Config, ca *pki.CA, nodeSvc *node.Service, cfgStore *configstore.ConfigStore, sessions *session.SessionManager, agentSrv *transport.Server, semantic *configstore.SemanticChecker, drift *configstore.DriftDetector, tmplSvc *configstore.TemplateService, deploySvc *deploy.Service, hub *Hub, certSvc *cert.Service, lvsSvc *lvs.Service, logStore logstore.Storage) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestLogger())
	r.Use(middleware.Audit(nodeSvc.Client()))

	r.GET("/health", func(c *gin.Context) {
		response.OK(c, gin.H{"status": "ok"})
	})

	// 写操作与接入令牌统一需 Bearer 鉴权（M1 最小可用安全策略）。
	auth := middleware.RequireAuth(cfg.AuthAdminToken)

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
			ns.GET("/:id/config-files", nh.ConfigFiles)
			ns.GET("/:id/log-targets", nh.LogTargets)

			// 写操作：需鉴权
			ns.POST("", auth, nh.Create)
			ns.POST("/:id/enroll-token", auth, nh.IssueEnrollToken)
			ns.POST("/:id/enroll-token/revoke", auth, nh.RevokeEnrollToken)
			ns.PATCH("/:id", auth, nh.Update)
			ns.DELETE("/:id", auth, nh.Delete)
			ns.POST("/:id/refresh", auth, nh.RefreshCapability)
		}

		// T043/T044/T045 证书管理：只读列表/详情 + 上传校验（6 项）+ 删除（级联清分发记录）
		// + 分发到节点（T044）+ ACME DNS-01 签发（T042/T045）+ 手动续期（T045）。
		// agentSrv 同时作为下发通道（cert.Deployer），实现私钥经 mTLS 下发、绝不进浏览器/DB 明文。
		ceh := handler.NewCertHandler(certSvc, agentSrv)
		ces := v1.Group("/certs")
		{
			ces.GET("", ceh.List)
			ces.GET("/:id", ceh.Get)
			ces.GET("/:id/deployments", ceh.Deployments)
			ces.POST("", auth, ceh.Upload)
			ces.POST("/issue", auth, ceh.IssueACME)
			ces.POST("/:id/distribute", auth, ceh.Distribute)
			ces.POST("/:id/renew", auth, ceh.Renew)
			ces.DELETE("/:id", auth, ceh.Delete)
		}

		// T021 配置中心：配置树版本化浏览（只读）。
		ch := handler.NewConfigHandler(cfgStore)
		cs := v1.Group("/configs")
		{
			cs.GET("", ch.ListByNode)                        // ?node_id=1 列出节点配置文件
			cs.GET("/:id", ch.GetFile)                       // 单文件（含当前版本内容）
			cs.GET("/:id/revisions", ch.ListRevisions)       // 版本链
			cs.GET("/:id/diff", ch.Diff)                     // ?from=&to= 两版语义 diff
			cs.POST("/:id/manual-edit", auth, ch.ManualEdit) // 编辑→保存为新版本（manual_edit）

			// T024 配置校验：触发目标 Agent 跑 nginx -t（写类操作，需鉴权）。
			vh := handler.NewValidateHandler(agentSrv)
			vh.SetSemanticChecker(semantic)
			cs.POST("/validate", auth, vh.Validate)
			// T025 语义校验：对节点当前配置跑规则引擎（读类分析，需鉴权）。
			cs.POST("/semantic-check", auth, vh.SemanticCheck)

			// T026 漂移检测：读报告免鉴权；手动提交 actual 触发检测需鉴权。
			dh := handler.NewDriftHandler(drift)
			cs.GET("/drift", dh.ListDrift)         // ?node_id=1 或全量
			cs.POST("/drift", auth, dh.CheckDrift) // 手动提交 actual 触发检测
		}

		// T027 配置模板与三级变量：模板浏览/渲染 + 变量管理（均含敏感信息，全部需鉴权）。
		th := handler.NewTemplateHandler(tmplSvc)
		ts := v1.Group("/templates")
		{
			ts.GET("", auth, th.ListTemplates)
			ts.GET("/:id", auth, th.GetTemplate)
			ts.POST("/:id/render", auth, th.RenderTemplate)
		}
		vs := v1.Group("/variables")
		{
			vs.GET("", auth, th.ListVariables)
			vs.POST("", auth, th.SetVariable)
		}

		// T030 发布引擎：变更单生命周期（创建/列表/详情 + 提交/批准/拒绝/取消）。
		dh := handler.NewDeployHandler(deploySvc)
		ah := handler.NewApprovalHandler(deploySvc) // T036 审批记录查询
		sh := handler.NewStreamHandler(hub)         // T037 SSE 实时进度推送
		do := v1.Group("/change-orders")
		{
			do.POST("", auth, dh.Create)
			do.GET("", dh.List)
			do.GET("/:id", dh.Get)
			do.POST("/:id/submit", auth, dh.Submit)
			do.POST("/:id/approve", auth, dh.Approve)
			do.POST("/:id/reject", auth, dh.Reject)
			do.POST("/:id/cancel", auth, dh.Cancel)
			do.POST("/:id/rollback", auth, dh.Rollback) // T039 发起回滚（执行随 Agent 落地）
			do.GET("/:id/approval", ah.GetForOrder)      // T036 取该变更单的审批记录
			do.GET("/:id/stream", sh.Stream)             // T037 SSE 实时进度
		}

		// T036 审批流：审批记录查询（列表按状态过滤）。
		as := v1.Group("/approvals")
		{
			as.GET("", ah.List)
		}
	}

	// T039 内嵌前端（仅 webui 构建生效；非 webui 构建为空操作）。
	web.RegisterWebUI(r)

	// T040 节点自注册分发：安装脚本 / 引导 CA / 二进制下载 / web 接入控制台 / Join Token 签发。
	registerAgentDistribution(r, ca, cfg.AgentDistDir, cfg.AgentGRPC, nodeSvc, auth)
	// 首跑管理员令牌获取（免 SSH+grep）：setup-token 免鉴权一次 / setup-acknowledge 锁定。
	registerAdmin(r, cfg, auth)

	// M5 LVS / Keepalived 拓扑与虚拟服务（只读聚合）。
	lvh := handler.NewLVSHandler(lvsSvc)
	v1.GET("/lvs/topology", lvh.Topology)
	v1.GET("/lvs/virtual-services", lvh.VirtualServices)
	// T054：RS 权重编排（摘除/恢复/设权），受 T055 门禁约束。
	v1.POST("/lvs/real-servers/:id/drain", lvh.Drain)
	v1.POST("/lvs/real-servers/:id/restore", lvh.Restore)
	v1.POST("/lvs/real-servers/:id/weight", lvh.SetWeight)
	// T056：脑裂检测（实时聚合 Director 持 VIP 态）。
	v1.GET("/lvs/split-brain", lvh.SplitBrain)

	// T063：日志检索 API（多维筛选 + 分页；依赖 T062 落库 Storage）。
	loh := handler.NewLogsHandler(logStore)
	v1.POST("/logs/search", loh.Search)
	v1.GET("/logs/trace/:request_id", loh.Trace)
	v1.POST("/logs/aggregate", loh.Aggregate)
	return r
}
