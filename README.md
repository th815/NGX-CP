# NGX-CP · Nginx 集群管理平台

> 把 Nginx 变更变成「可校验、可灰度、可观测、可回滚」的流水线，而不是又一个配置分发工具。

面向**裸机 / VM 上的 Nginx 集群**（非 K8s Ingress 场景），并支持 **LVS+DR** 架构的 LVS 层编排。
适用于「2 台 Keepalived（主备）+ 2 台 Nginx RS（DR 模式）」这类自用规模，也预留了百万级日访问的容量余量。

## 特性（规划 / 落地中）

- 多节点纳管（Agent 主动外连，gRPC + mTLS，节点无需开入站端口）
- 配置检查 / 更新 / 修改 / 同步（模板 + 三级变量：节点 > 集群 > 全局）
- 证书管理 + 同步（手动上传 / ACME DNS-01，私钥加密不下发浏览器）
- 配置备份与版本血缘（内容寻址 blob + revision 链）
- LVS（DR 模式）配置管理 + 无损发布（权重摘除式灰度，zero 5xx）
- 统一日志 + 攻击预警（TraceID 全链路、ClickHouse 检测即 SQL、封禁复用发布流水线）

## 架构

- 后端：Go（Gin 风格）+ `embed.FS` 内嵌前端，单二进制 systemd 部署
- Agent：常驻节点，主动外连控制面（mTLS），内建传输与 tail，**无远程命令执行**
- 数据库：PostgreSQL 16 主库（开发态可用 SQLite 同构 fallback），无 Redis
- 时序 / 日志：ClickHouse 单实例（限 6G 内存 + TTL 7 天）
- 监控：Prometheus + Grafana 直接用，平台只自研业务视角指标与告警汇聚

完整设计见 [`docs/`](docs/)：`PRD.md` / `ARCHITECTURE.md` / `DECISIONS.md`；
任务拆解见 [`docs/tasks/`](docs/tasks/)（M0–M9，约 70 个 AI 任务）。

## 状态

- ✅ **M0 地基**：模块结构、配置、错误封装、日志、ent schema + 双 DB 迁移，全部验收通过。
- ✅ **M1 接入层**：Agent 主动外连（gRPC + mTLS）、注册 / 心跳 / 能力发现。
- ✅ **M2 配置中心**：配置树、版本链、Diff、校验、漂移检测。
- ✅ **M3 发布引擎（核心）**：变更单状态机(T030)、发布前快照(T031)、原子落盘(T032)、探活(T033)、回滚(T034)、LVS 权重摘除式灰度(T035)、审批流(T036)、SSE 实时推送(T037)、并发控制与任务队列(T038)、**T039 发布页面与集成验收** 全部完成 —— 已达成「最小可用闭环」。
- ✅ **Agent 执行闭环接线**：控制面经心跳命令通道下发 部署/回滚/快照/调权 指令（transport 层），
  AgentRunner 实现 `deploy.Runner` 并接入 worker —— 变更单从「等待执行器接入」收敛为真实 success/failed，
  不再假装成功。附带修复 watcher 注册竞态（监听生效前的配置变更会被永久丢弃）。
  落地工具：`scripts/deploy-agent.sh`（rollback-safe：备份→stop→落位→start→校验，
  失败自动回滚；主机/IP/令牌全部由环境变量与令牌文件提供，无内置环境信息）+ systemd 单元。
- 🌐 **节点自注册（控制台一键，推荐）**：在**「节点」页面 `/nodes`** 点「添加节点」—— 填节点名 + 选角色 + 选有效期
  →「登记并生成安装命令」，签发**节点绑定 Join Token**（服务端 `join_tokens` 表，可单独吊销），
  显示 `curl … | bash` 一行命令（控制面地址与 gRPC 端口由 `GET /api/v1/agent/bootstrap-info` 自动填充）；
  独立页 `/agent/` 保留为备用入口。控制面须启用二进制分发（`agent_dist_dir`）：`deploy.sh` 自动构建上传并幂等补齐
  配置；存量控制面可跑 `scripts/enable-agent-dist.sh` 一次性补救（修复 `[2/5] 下载 Agent 二进制 404`）。
  节点卡片提供 **详情 / 接入命令（重新签发） / 编辑 / 删除**；管理员令牌在顶栏右上角「管理员令牌」框
  或「系统设置」页录入（API 401 时自动弹窗引导填写），系统设置页另提供连通性自检与分发状态。
  节点自拉二进制 + CA、装 systemd、用 Join Token + 本地 CSR 自注册，**复用既有节点**无审批直接上线
  （令牌持久化于 Agent 侧 `/etc/ngxcp/agent.conf`）。`POST /api/v1/nodes` 新建并签发，`POST /api/v1/nodes/:id/join-token` 轮换（吊销旧令牌），亦可经 `POST /api/v1/nodes/:id/join-token/revoke` 独立吊销（只吊销不签发，安全事件响应）。
  另保留**预建节点一次性 Enroll Token**路径（`POST /api/v1/nodes/:id/enroll-token`，可经 `POST /api/v1/nodes/:id/enroll-token/revoke` 主动吊销），
  同样入库 `enroll_tokens` 表、**持久化（重启不丢）且可主动吊销**。两类令牌控制面都只存 SHA-256 哈希 + 绑定节点 + 过期 + 吊销/已用标志，原文仅签发时返回一次。
  旧的手工 `enroll-agent.sh` 已移除（那套「先建节点再逐台发令牌」不是自注册）。
  批量一次性纳管 2+2 现成脚本：`scripts/enroll-cluster.sh`（免手动取令牌——自动从控制面读 `auth_admin_token` 并去 YAML 引号；建节点 + 签发一次性 Enroll Token + 调 `deploy-agent.sh` 一条命令完成；节点清单/控制面地址见脚本顶部可配置区）。
  两条部署路径（enroll-cluster 批量 / Web 一键 install_agent.sh）均显式注入 `server-name=ngxcp-server`，以匹配控制面服务端证书 SAN（pki/ca.go 固定为 ngxcp-server/localhost）；缺失该值会导致 Agent mTLS 握手 SAN 不匹配、注册即退出、systemd 启动校验失败。
  单元启用 `ProtectSystem=full`：systemd 启动时会把 `ReadWritePaths`(/etc/nginx /etc/keepalived /var/lib/ngxcp /var/log/nginx) bind mount 进服务命名空间，**路径不存在则命名空间搭建失败（exit 226/NAMESPACE）、Agent 二进制执行前即死**。Director 节点无 nginx 故缺 `/etc/nginx`、`/var/log/nginx`——部署脚本现已 `mkdir -p` 预建这些目录，并落 `tmpfiles.d/ngxcp-agent.conf`（开机早期建目录，防重启后 Agent 起不来）。
  **生产落地（2026-09-07）**：`192.168.5.50` 控制面已部署，`192.168.5.6/.7`(director) + `192.168.5.8/.9`(real_server) 4 台经 `enroll-cluster.sh` 全量 `online`、角色正确——最小可用闭环首次在真机跑通，M0–M3 不再只是引擎级闭环。
  **节点删除 5000 修复（2026-09-07）**：`DELETE /api/v1/nodes/:id` 原直接硬删节点，被 ent 的 RESTRICT 外键拦下（残留 enroll_tokens 等子记录）→ 报 `{"code":5000}` 删不了。已在 `node.Service.Delete` 内改事务级联清理 8 张子表（enroll_tokens / join_tokens / node_capabilities / node_config_files / node_log_targets / config_snapshots / deploy_tasks / real_servers）再删节点本体，附单测 `TestDeleteCascade` 锁定不回归。残留的 `nginx-rs-01`(id=1) 现可正常删除。
  **首跑令牌免 SSH 获取（2026-09-07）**：针对"生产谁会去 grep auth_admin_token"的痛点，新增首次设置通道——`GET /api/v1/admin/setup-token`（免鉴权，仅未确认时返回当前令牌一次）+ `POST /api/v1/admin/setup-acknowledge`（凭有效令牌确认，落标记文件后前者改返回 410 锁定）。Web「系统设置」页与 401 弹窗首跑自动显示一次性明文令牌，点「完成首次设置」即锁定，全程无需登服务器。控制面启动未确认时打 WARN 日志提示。信任边界：仅"未确认前"暴露，与 config 文件本地可读一致。
  **M4 证书管理 · T043 手动上传+6 项校验（2026-09-12）**：完成证书安全库存闭环——`internal/cert/validate.go` 实现 6 项校验（私钥/证书模数匹配、链完整+顺序不含 root、SAN 覆盖、有效期过期拒/<7天警告、弱签名算法拒绝），单测 `validate_test.go` 覆盖 6 类失败样本+成功样本；`internal/domain/cert/service.go` 上传走校验+KMS 信封加密入库（私钥/链 AES-GCM 加密，API 永不回传），并事务级联删分发记录；`internal/server/handler/cert.go` + 路由 `/api/v1/certs`（GET 列表/详情、POST 上传校验、DELETE 级联）+ server 注入 KMS；前端 `views/certs/Certs.vue`（列表到期色阶、上传表单实时回显 6 项结果）。`deploy.sh` 补齐生成 `/etc/ngxcp/master.key` 主密钥。`service_test.go` 锁定加密入库+级联删除。vue-tsc/vite build/go test 全过。
  **M4 证书管理 · T044 证书分发到节点（2026-09-12）**：彻底告别手动 scp 证书。proto 新增 `DEPLOY_CERT` 指令 + `DeployCertTask`/`DeployCertResult` 消息（buf 重新生成）；Agent 端 `executor/cert_deploy.go` 走 staging→同盘 rename 原子切换→chmod(crt 0644/key 0600)→nginx -t→reload→探活(443 TLS) 的落盘流水线（单测锁定权限与回滚），并由 `heartbeat.go` + `runtime.go` 接入（仿 SET_RS_WEIGHT 同构）；控制面 `transport.Server.DeployCert` 经心跳命令流下发并回收结果；`cert.Service.Distribute` 解密 KMS 后逐节点下发、写/更新 `cert_deployments` 分发记录（`service_test.go` 锁定加密回放+成功/失败双路径）；`handler/cert.go` 加 `POST /:id/distribute` + `GET /:id/deployments`；前端 `Certs.vue` 加「分发」按钮→节点多选弹窗→逐节点结果展示。安全红线守住：私钥明文仅在本请求作用域经 mTLS 下发，浏览器与控制面 DB 均不留存。`go build`/`go test`/vue-tsc/vite build 全过。
  **M4 证书管理 · T045 自动续期调度（2026-09-13）**：把"证书会过期"变成无人值守的系统行为。`ent/schema/certificate.go` 增 6 个 ACME 续期字段（加密的账户密钥/provider Token、provider 类型、邮箱、密钥算法、CA 目录 URL）持久化续期凭据；`internal/domain/cert/acme.go` 实现 `IssueACME`（签发即入库续期凭据）与 `Renew`（复用存储账户密钥重签发→更新库内证书行→走 T044 `Distribute` 原子流水线重分发到历史节点，满足"续期必须走发布流水线"）。`renew_scheduler.go` 是仅依赖 Renewer/DueLister/Recorder 三小接口的调度器：`NewScheduler` 默认 horizon=30 天；`Start` 首次对齐当日 03:00、之后每 24h 触发；`RenewDue` 遍历到期证书，连续失败达 3 次经 `onCritical` 升级 CRITICAL 告警，每次结果经 `Recorder` 落一条 `source=auto_renew`/`type=cert_renew` 变更单作审计（单测 `renew_scheduler_test.go` 锁定触发/连败升级/成功复位/到期筛选/续期重分发）。后端接线：`handler/cert.go` 加 `POST /api/v1/certs/issue`（ACME 签发）与 `POST /api/v1/certs/:id/renew`（手动续期）；`server.go` 给 `certSvc.SetDeployer(agentSrv)` 并在主密钥就绪时启动调度 goroutine、未配 KMS 则跳过并告警；`renewRecorder`（`renewRecorder`/`certDueLister` 适配器）把每次续期驱动为 draft→pending→running→success/failed 审计单。前端 `Certs.vue` 加「签发 ACME」弹窗（域名/邮箱/provider Token/算法/CA 目录；provider 下拉仅 Cloudflare 可选且已在注册表，其他标"规划中"）与每行「续期」按钮（仅 ACME 来源）。`go build`/`go vet`/`go test`/vue-tsc/vite build 全过。`web/src/api/cert.ts` 增 `issueACME`/`renewCert`。**未真机验证**：lego 真实签发、Cloudflare Token 真机、pebble 调试均未在沙箱验证，续期调度仅经单测 + 编译验证。
- 🟢 **M4 证书管理**：T040 数据模型+加密存储 ✅、T043 手动上传+6 项校验+UI ✅、T044 证书分发到节点 ✅、T041 DNS Provider 抽象+Cloudflare ✅、T042 ACME DNS-01 自动签发(通配符) ✅、T045 自动续期调度 ✅（每日 03:00 触发、到期前 30 天续期、续期复用 T044 原子流水线重分发 + 落 `cert_renew` 审计单、连续失败升级告警；依赖 Cloudflare token + pebble 真机验证）。
- 🟢 **M5 LVS 管理 · 基础+可视化闭环（2026-09-12）**：补齐「全流程」里的 LVS 拓扑可视一环。T050 `ent/schema/director.go`(state/priority/vrid/unicast_src/peer/iface/mode/vip/holding_vip + Node 反向边) + `ent/schema/virtual_service.go`(director_id/vip/port/protocol/scheduler/enabled) + Node 显式级联删 directors（保护已验证的节点删除逻辑）；T051 `internal/lvs/render.go` Keepalived 渲染器（global_defs + vrrp_instance VI_1 + 每 VS 的 virtual_server/real_server，主备仅 state/priority/unicast 三项不同，vrid 冲突检测），`render_test.go` 锁主备差异/同VIP多端口/VRID冲突/禁用RS省略；T053 `internal/lvs/topology.go` 聚合 Director+VS+节点级 real_server+在线状态为 `Topology`，`handler/lvs.go` 暴露 `GET /api/v1/lvs/topology`（只读），`router.go` 注入 lvs svc；T057 `views/lvs/Topology.vue` + `components/lvs/TopoSvg.vue`（VIP→主/备 Director→RS×2 拓扑 SVG，健康/持有色阶）+ `api/lvs.ts` + 路由接入。`topology_test.go` 锁聚合。go build/go test/vue-tsc/vite build 全过。
- 🟢 **M5 LVS 管理 · T054 权重灰度发布编排 + T055 节点门禁（2026-09-12）**：控制面把已就绪的 Agent `SET_RS_WEIGHT` 通道接成 `domainlvs.WeightSetter`——`internal/lvs/remote.go` `RemoteSetter`（经 ent 把 VS 的 vip/port 映射到 Director 节点 ID，再调 `transport.Server.SetRSWeight` 下发 `SetRealServerWeightTask`）+ `ResolveDirectorNode`；`topology.go` `Service` 暴露 `Drain`/`Restore`/`SetBaselineWeight` 编排（按 rip 聚合多条 RS 记录、门禁逐节点检查、下发后写回模型、保留基线），`RealServerNode` 加 `id` 供前端调权 API；`internal/lvs/gate.go` `Gate.Check` + `NodeStatusCompliance`（degraded/offline/decommissioned 拒绝参与 LVS 发布，纵深防御）；`handler/lvs.go` 新增 `POST /api/v1/lvs/real-servers/:id/{drain,restore,weight}`，`router.go` 注入，`server.go` 装配 `SetWeightSetter`+`SetGate`；前端 `Topology.vue` 加「RS 权重编排」卡（摘除/恢复/设权 0–100），`api/lvs.ts` 加 `drainRealServer`/`restoreRealServer`/`setRealServerWeight`。单测 `service_test.go`(fakeSetter 锁 drain/restore/setWeight/门禁拦截 Agent 调用) + `gate_test.go` 全过。go build/go test/vue-tsc/vite build 全过。**设计遗留**：`RemoteSetter.ListVirtualServers` 暂返回 error（`LIST_VS` 运行时查询通道待建），故完整 7 步 `GracefulDeploy` 的排空检测（ActiveConn）尚不可用于分布式环境；当前交付为「按模型驱动的权重编排 + 门禁」，满足 T054-A/B 与 T055 范围。T055 完整门禁（接真实合规上报）与 T052 合规自检绑定，待后续。**未真机验证**：Agent 实际调权、lego 真实签发均未经真机/pebble 验证。
- - 🟢 **M5 LVS 管理 · T052 合规自检接线 + T055 门禁接真实合规（2026-09-08）**：补齐"本该有而没有"的缺口——Agent 运行时的 `HeartbeatCallbacks` 此前**从未赋值** `ReportCompliance`/`ReportFsProbe`，致 DR 合规自检与 FS 探测引擎虽就绪却**永不在真机触发**（`RUN_COMPLIANCE` 命中 nil 回调变空操作）。`internal/agent/runtime/runtime.go` 现接入 `onReportCompliance`/`onReportFsProbe`（调用既有 `health.RunCompliance`/`RunFsProbe`，输入来自 `cfg` 与 `hostexec`）；`cmd/ngxcp-agent/main.go` 新增 `--vips`（env `NGXCP_AGENT_VIPS`，逗号分隔）供 `vip_on_lo` 校验（空则跳过该项）；`heartbeat.go` 新增周期合规自检 goroutine（复用 `FsProbeInterval`，满足"每 5 分钟"），与 `RUN_COMPLIANCE` 指令**双路径**均上行 `COMPLIANCE`。控制面 `SetCompliance`→`recomputeHealth`→节点 `degraded` 链路早已就绪，现由真实数据驱动，LVS 发布门禁（T055 `Gate`/`NodeStatusCompliance`）据此拦截——T055 即"接上真实合规上报"。`go build`/`go vet`/`go test ./internal/agent/...`（含 `TestHeartbeaterReportsComplianceAndFsProbe`）全过。**未真机验证**：Agent 真实执行 `ip`/`sysctl`/`keepalived.conf` 探测、lego/pebble 均未在沙箱验证。
- 🟢 **M5 LVS 管理 · T056 脑裂监测 + vCenter 端口组强制清单（2026-09-08）**：补齐 M5 最后一环。① proto 离线补 `ComplianceReport.holding_vip`（field 5，bool）——无 protoc/buf 时用手写解析器改写 `rawDesc` 序列化 `FileDescriptorProto` 并重写常量（保留 struct tag 与 getter），单测 `rawdesc_patch_test.go` 锁 wire 往返；② Agent `health.RunCompliance` 增 `checkHoldingVIP`：对 Director 角色，任一配置 VIP 绑在「非 lo」接口即 `HoldingVip=true`（RS 的 VIP 在 lo 故天然 false），单测覆盖持 VIP/仅 lo/无 VIP/探测失败四态；③ 控制面 `node.Service.SetCompliance` 把 `holding_vip` best-effort 回写关联 `Director.HoldingVip`；④ `internal/lvs/split_brain.go` 新增 `DetectSplitBrain`（≥2 Director 同时持 VIP→脑裂，纯函数单测）+ `CheckSplitBrain`（聚合 DB 全部 Director 实时态）+ `StartSplitBrainWatch`（周期 1min，检测到打 CRITICAL 日志），并在 `server.go` 启动 watch goroutine（与进程同生命周期）；⑤ `handler/lvs.go` + `router.go` 新增 `GET /api/v1/lvs/split-brain`；⑥ 新建 `deploy/checklist.md` 固化 vCenter 端口组三项强制项（混杂模式 / MAC 地址更改 / 伪传输）。`go build`/`go vet`/`go test ./internal/lvs/... ./internal/agent/health/... ./gen/agent/v1/...` 全过。**未真机验证**：Agent 真实 `ip addr` 探测、lego/pebble、vCenter 端口组均未经真机验证；脑裂判定按布尔聚合（新鲜度由合规上报周期与 watch 间隔保证），未引入额外时间戳列。
- 🟢 **M6 日志与安全 · T061 Agent 日志采集模块（2026-09-08）**：落地日志采集引擎 `internal/agent/logtail/`（line/offset/queue/tail 四文件）。`Tailer.Run(ctx, emit)` 从持久化 offset 续读、监控 inode 变化应对 logrotate（旧 fd 读完 EOF 后重开 path）、同 inode 截断重置 offset、按 `SampleRate` 降采样（4xx/5xx 与无法解析行恒保留）、攒批（BatchSize/FlushEvery）调用注入式 `emit`；`emit` 失败进本地磁盘队列 store-forward（JSONL、保留 24h、启动时回放），offset 用 tmp+rename 原子写。`tail_test.go` 锁解析/采样/offset 重启续读/logrotate(inode 变更)续读/截断重置/emit 失败 store-forward 补传六类。`go build`/`go vet`/`go test ./internal/agent/...` 全过。**设计边界**：本模块只负责可靠喂给 `emit`，下游经控制面上报到 ClickHouse 由 T062 实现，故 T061 未接入 runtime（避免半接线）；Agent 实际落盘采集待 T062 控制面 ingestion 就绪后接线。
- 🟢 **M6 日志与安全 · T062 ClickHouse 入库引擎（2026-09-08）**：落地日志落库与攒批引擎 `internal/logstore/`（entry/convert/schema/ingester/clickhouse 五文件）。`Entry` 为规范化记录（字段对齐 T060 日志格式与 T061 LogLine）；`Storage` 接口依赖倒置——生产 `ClickHouseStorage`（`clickhouse-go/v2` 经 `PrepareBatch+Append+Send` 批量写入，连接级 `max_memory_usage` 经 `ParseMemLimit` 解析 "6G" 设 6G，建表 `ApplySchema` 幂等 `IF NOT EXISTS`，DDL 见 `deploy/clickhouse/init.sql`）+ 开发/测试 `MemStorage`；`Ingester` 攒批（`batchSize` 默认 1000 / `flushEvery` 默认 5s，达量即 flush，周期 flush，Close 刷余，入库失败重缓冲不丢行）。`clickhouse_test.go` 锁转换/达量 flush/周期 flush/关闭刷余/失败重缓冲/内存上限解析七类。`go build`/`go vet`/`go test ./internal/logstore/...` 全过。**未接线 server**：传输（Agent gRPC 上报→Ingester.Accept）属 T063，本模块只做引擎，避免半接线；生产 ClickHouse 实例与真机写入未验证。
- 🟢 **M6 日志与安全 · T063 日志检索 API（2026-09-08）**：`Storage` 接口新增 `Query(ctx, QueryParams)(*QueryResult,error)`；`query.go` 做参数化查询构造（`buildWhere` 全 `?` 占位杜绝注入，`escapeLike` 转义 LIKE 通配符，`match` 内存实现与 CH 语义一致，默认近 24h 时间窗防全表扫）；`ClickHouseStorage.Query` 走参数化 SQL + 计数查询；`handler/logs.go` + `router.go` 暴露 `POST /api/v1/logs/search`（多维筛选 status/nodes/uri/ip/rid/rt_min + regex 开关 + 分页，返回 `{code,data:{items,total,took_ms}}`）；`server.go` 按 `NGXCP_LOGSTORE_DSN` 构造 `ClickHouseStorage`（缺省回落 `MemStorage`，不阻断启动）。`query_test.go` 锁参数化/escapeLike/MemStorage 多维过滤+分页+默认时间窗、`logs_test.go` 锁 httptest 端点（status/空结果/坏体）共 11 类全过。`go build`/`go vet`/`go test ./internal/logstore/... ./internal/server/handler/...` 全过。**设计边界（延续）**：生产 ClickHouse 实例与真机写入未验证。
- 🟢 **M6 日志与安全 · T063-补 日志传输接线（2026-09-08）**：把 T061→T062→T063 闭合成端到端管道。离线 patch `gen/agent/v1/agent.pb.go` 的 `rawDesc`：给 `HeartbeatRequest.Type` 枚举加 `LOG_BATCH=12`、给 `HeartbeatRequest` 加字段 14 `log_batch`（`bytes`，避免新增消息类型牵动 msgTypes 注册表——沿用 T056 的 bytes 同构手法）；新增 `rawdesc_logbatch_test.go` 锁枚举/字段/字节往返。`internal/agent/logtail/batch.go` 加 `MarshalBatch`/`UnmarshalBatch`（`[]LogLine` 经标准 JSON 批次序列化，TS 无效时按零值不丢行）。Agent 侧：`heartbeat.go` 的 `HeartbeatCallbacks` 加 `StartLogTail`，`session()` 起 goroutine 经 `logBatchOut` 周期上行 `LOG_BATCH`；`runtime.go` 注入 `startLogTail`（用 `CollectLogTargets` 取非 off/variable/syslog 目标，每目标起 `Tailer` 并对 `[]LogLine` 打包 emit）。控制面侧：`grpc_server.go` 加 `LogAcceptor` 接口（`Accept([]logstore.Entry)`）+ `SetLogIngester`，`Heartbeat` 消费 `req.GetLogBatch()`→`logtail.UnmarshalBatch`→`logstore.FromLogLines`→`logIngester.Accept`；`server.go` 建 `ingester:=logstore.NewIngester(logStore)`、`Start(ctx)` 并 `agentSrv.SetLogIngester(ingester)`。`heartbeat_test.go` 加 `TestHeartbeaterReportsLogBatch`（断言流收到 `LOG_BATCH` 且含行），`grpc_server_test.go` 加 `TestHeartbeatLogBatchIngested`/`TestHeartbeatLogBatchEndToEnd`（LOG_BATCH→Ingester→MemStorage→`Query(status=503)` 命中），共端到端可验证。`go build`/`go vet`/`go test ./internal/... ./gen/...` 全过。**未真机验证**：Agent 真实落盘 tail、生产 ClickHouse 写入、真机 mTLS 上报均未验；检索 API 现已对 Agent 上报数据生效（受 `NGXCP_LOGSTORE_DSN` 控制，缺省 `MemStorage`）。
⬜ **M6–M9**：日志与安全 / 监控 / 构建升级 / 备份运维（增值模块，可边用边做）。T060/T064–T070 待做：标准日志格式下发(T060)、TraceID 全链路追踪(T064)、聚合分析 API(T065)、攻击检测规则引擎(T066)、告警中心(T067)、封禁变更单(复用发布流水线)(T068)、分级处置(T069)、日志/安全 UI(T070)。

> 完成 M0–M3 即达成「最小可用闭环」：已能安全地把配置变更做成「可校验、可灰度、可观测、可回滚」的流水线，可投入实际使用再迭代。

## 快速开始（开发）

```bash
# 1. 起开发用 PostgreSQL（可选，默认走 SQLite 同构）
docker compose up -d postgres

# 2. 编译（产出 linux/amd64 静态二进制到 bin/）
make build
#    本地 macOS 验证可直接：
go run ./cmd/ngxcp-server --check-config

# 3. 建表（双路：sqlite / postgres 均可）
NGXCP_DB_DRIVER=sqlite NGXCP_DB_DSN="file:./dev.db?_fk=1" make migrate-dev

# 4. 跑测试
make test
```

配置项见 [`configs/config.example.yaml`](configs/config.example.yaml)，可被 `NGXCP_<KEY>` 环境变量覆盖。

## API 文档（Apifox）

接口契约以 OpenAPI 3 维护在 [`api/openapi.yaml`](api/openapi.yaml)。
在 **Apifox** 中：「项目 → 导入 → OpenAPI / Swagger」选择该文件，即可生成可调试的 API 文档与 Mock。
后续 M1+ 新增接口时扩展该文件，并在 Apifox 用「覆盖导入」刷新。

当前已暴露：`GET /health`、`GET /api/v1/version`、`GET /api/v1/nodes`（占位空列表）。

## 原型

方案阶段的高保真交互原型见 [`prototype/index.html`](prototype/index.html)（单文件、无外部依赖，双击即开，数据为模拟数据）。

## 说明

- Go module 当前为 `github.com/th/ngxcp`，会在 v1 前对齐到仓库路径 `github.com/th815/NGX-CP`。
- 本地项目数据（`.workbuddy/`）、密钥、数据库文件均已加入 `.gitignore`，不会进入仓库。

## License

[Apache-2.0](LICENSE)
