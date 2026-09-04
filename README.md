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
- ⬜ **M4–M9**：证书管理 / LVS 管理 / 日志与安全 / 监控 / 构建升级 / 备份运维（增值模块，可边用边做）。

> 完成 M0–M3 即达成「最小可用闭环」：已能安全地把配置变更做成「可校验、可灰度、可观测、可回滚」的流水线，可投入实际使用再迭代。

- 🚀 **已部署**：控制面 `ngxcp-server` 已正式部署至 `192.168.5.50`（Rocky Linux 9，systemd 托管，SQLite 单实例，**单二进制内嵌前端 SPA**）。部署脚本 `scripts/deploy-5.50.sh`、单元 `scripts/ngxcp-server.service`、记录见 [`docs/DEPLOY-192.168.5.50.md`](docs/DEPLOY-192.168.5.50.md)。T039 落地后 `/`、`/deploy` 等深链已返回 Web 界面，发布页可对变更单做建单/审批/回滚并实时看进度；**生产态 `Runner` 为 nil（Agent 尚未下发到 Nginx 节点 5.8/5.9，属生产只读环境待授权）—— 闭环在「审批通过→等待执行器」处按设计暂停**，待 M4+ 部署 Agent 后订单自动收敛 `success`/`failed`。

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

[MIT](LICENSE)
