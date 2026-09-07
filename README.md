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
- 🟡 **M4 证书管理**：T040 数据模型+加密存储 ✅、T043 手动上传+6 项校验+UI ✅、T044 证书分发到节点 ✅；T041 DNS Provider / T042 ACME 自动签发 / T045 自动续期 待做（依赖 Cloudflare token + pebble 测试环境）。
- ⬜ **M5–M9**：LVS 管理 / 日志与安全 / 监控 / 构建升级 / 备份运维（增值模块，可边用边做）。

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
