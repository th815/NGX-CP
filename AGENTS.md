# AGENTS.md — NGX-CP 项目宪法

> **本文件是给 AI 编程助手读的第一份文件。每个新会话开始前必读。**
>
> 本项目由 AI 全程编写。这份文件的作用是：让每一个新的 AI 会话在没有历史上下文的情况下，也能产出与已有代码风格一致、符合架构约束、可验证的代码。

---

## 0. 三十秒理解这个项目

**NGX-CP（Nginx Control Plane）** 是一个自用的 Nginx 集群管理平台，管理 **2 台 Keepalived Director（主备 / LVS-DR）+ 2 台 Nginx RealServer**。

一句话定位：

> 把 Nginx 配置变更变成一条 **可校验 → 可灰度 → 可观测 → 可一键回滚** 的流水线。

三根支柱：
1. **一处改全局达** —— 模板 + 三级变量，杜绝配置漂移
2. **先验证再下发** —— `nginx -t` + 能力基线校验 + LVS 权重摘除式灰度
3. **随时能退回去** —— 发布前自动快照 + 版本链，回滚 ≤ 60 秒

**不是**批量改配置文件的工具。**是**变更流水线。这个区别决定了所有设计决策。

---

## 1. 技术栈硬约束（不得替换）

**这些选型已经过论证，记录在 `docs/DECISIONS.md`。AI 不得提议替换、不得"顺手优化"成别的方案。**

| 层 | 选型 | 版本 | 备注 |
| --- | --- | --- | --- |
| 语言 | **Go** | 1.22+ | 控制面与 Agent 共用 |
| 前端 | **Vue 3 + TypeScript + Naive UI + Monaco Editor** | Vue 3.4+ | Monaco 提供 nginx 语法高亮与 Diff |
| API 风格 | REST（内部）+ gRPC（Agent） | — | OpenAPI 3.1 契约先行 |
| ORM | **ent**（entgo） | 0.13+ | schema-as-code，自动生成 CRUD 与 migration |
| 数据库 | **PostgreSQL 18** | — | 开发态可用 SQLite，见 §6 |
| 日志存储 | **ClickHouse** | 24.x | 访问日志，TTL 90 天 |
| 指标存储 | **VictoriaMetrics**（单实例） | 1.102+ | 兼容 PromQL，`-retention.period=1y` |
| 可视化 | **Grafana** | 11.x | Auth Proxy 嵌入；面板 JSON 纳入版本管理 |
| 告警 | **vmalert + Alertmanager** | — | 不得自研告警路由 |
| 配置 | **viper** | — | 环境变量优先 |
| 日志 | **zerolog**（结构化 JSON） | — | 见 §5 |
| 测试 | **testify + testcontainers** | — | 见 §7 |
| Agent | **Go 单二进制**，静态编译，无 CGO | — | 零依赖，不要求节点装任何东西 |

**明确排除的组件**（不要提议引入）：

- ❌ Redis —— 用进程内 goroutine + 事务乐观锁
- ❌ Kafka / RabbitMQ —— 用 PostgreSQL 的 `SKIP LOCKED` 做任务队列
- ❌ Kubernetes / Docker（业务节点上）—— 节点是裸 nginx
- ❌ 独立的时序数据库产品（InfluxDB / TDengine 等）—— VictoriaMetrics 本身就是
- ❌ Filebeat / Vector / Logstash —— 采集做进 Agent
- ❌ Prometheus（用 VictoriaMetrics 替代，但指标格式仍是 Prometheus 文本格式）
- ❌ scp —— 已 deprecated，见 DECISIONS §1

---

## 2. 目录结构（不得随意新增顶层目录）

```
ngxcp/
├── AGENTS.md                    # 本文件
├── Makefile                     # 所有常用命令入口
├── go.mod
├── docker-compose.yml           # 中间件（PG / ClickHouse / VM / Grafana）
│
├── cmd/
│   ├── ngxcp-server/            # 控制面入口
│   └── ngxcp-agent/             # Agent 入口
│
├── internal/
│   ├── server/                  # HTTP 服务、路由、中间件
│   │   ├── router.go
│   │   ├── middleware/          # 鉴权、审计、限流
│   │   └── handler/             # 按资源分文件：node.go / config.go / deploy.go ...
│   ├── agent/                   # Agent 侧逻辑
│   │   ├── capability/          # nginx -V / -T 解析  ★
│   │   ├── executor/            # 原子落盘 / reload / 热升级
│   │   ├── collector/           # 日志采集（tail + offset + 队列）
│   │   ├── metrics/             # 滑窗聚合 → /metrics
│   │   └── probe/               # DR 合规自检、探活
│   ├── domain/                  # 领域模型与业务逻辑（不依赖传输层）
│   │   ├── config/              # 配置树、版本链、diff、校验
│   │   ├── deploy/              # 发布引擎状态机
│   │   ├── cert/                # 证书签发、校验、分发
│   │   ├── lvs/                 # Keepalived 渲染、ipvsadm 抽象、DR 合规
│   │   ├── security/            # 攻击规则引擎、封禁
│   │   ├── build/               # 编译构建与热升级
│   │   └── notify/              # 告警通知
│   ├── repo/                    # 数据访问层（ent 生成 + 手写复杂查询）
│   ├── pkg/                     # 可复用工具（无业务依赖）
│   │   ├── nginxconf/           # nginx 配置解析器
│   │   ├── atomicfile/          # 原子落盘
│   │   └── shellx/              # 命令执行封装（超时、输出捕获）
│   └── config/                  # 配置加载
│
├── ent/
│   ├── schema/                  # ent schema 定义（唯一事实来源）
│   └── generate.go
│
├── proto/agent/v1/agent.proto   # 控制面 ↔ Agent 契约（唯一事实来源）
├── gen/                         # 生成代码（proto / ent），不手写
│
├── web/                         # 前端
│   ├── src/
│   │   ├── api/                 # API 客户端（按资源分文件）
│   │   ├── views/               # 页面组件
│   │   ├── components/          # 通用组件
│   │   ├── stores/              # Pinia
│   │   └── router/
│   └── package.json
│
├── configs/                     # 配置模板与数据文件
│   ├── module_matrix.json       # nginx 版本 × 模块兼容矩阵  ★
│   ├── log_format.conf          # 统一下发的 JSON 日志格式
│   ├── nginx_directives.json    # 指令废弃/变更表
│   └── grafana/provisioning/    # Grafana 面板 JSON（纳入版本管理）
│
├── scripts/
│   ├── backup.sh
│   └── restore.sh
│
└── docs/
    ├── PRD.md
    ├── ARCHITECTURE.md
    ├── DECISIONS.md            # 全部架构决策与论证
    └── tasks/                  # AI 任务清单（分里程碑）
        ├── README.md
        └── M0-foundation.md ...
```

**规则**：
- 新增文件前先确认是否属于现有目录；确需新目录时在 PR 里说明理由
- `gen/` 下所有文件是生成的，AI 不得手写或修改
- `ent/schema/` 改了必须跑 `go generate ./ent`

---

## 3. 代码风格

### 3.1 Go

```go
// 命名
// - 接口用 -er 后缀或不带 I 前缀：Reader, ConfigStore（不用 IConfigStore）
// - 错误变量：ErrXxx；哨兵错误放包顶部
// - 常量组用 iota 时第一个要有显式类型

// 函数长度：单函数不超过 60 行，超了就拆
// 文件长度：单文件不超过 400 行，超了就按职责拆

// 注释：导出标识符必须有注释（golint 要求）；复杂逻辑块上方写"为什么"而不是"是什么"

// 接收者命名：统一用单字母且与类型一致，不要混用 this/self
func (s *ConfigService) Validate(ctx context.Context, id int) error { ... }
```

**强制格式化**：提交前跑 `make fmt`（`gofmt -w` + `goimports -w -local github.com/th/ngxcp`）。

**Lint**：`golangci-lint run`，规则见 `.golangci.yml`。禁止用 `//nolint` 绕过，除非在注释里写明理由。

### 3.2 前端

- 组件用 `<script setup lang="ts">`
- 样式用 scoped CSS，不引入 CSS 框架（Naive UI 已有主题）
- API 调用统一走 `src/api/`，不在组件里写 `fetch`
- 类型定义集中在 `src/api/types.ts`，由 OpenAPI 生成，不手写

### 3.3 提交信息

```
<type>(<scope>): <subject>

type: feat / fix / refactor / test / docs / chore / perf
scope: node / config / deploy / cert / lvs / security / build / monitor / agent / web

例：
feat(deploy): 实现 LVS 权重摘除式灰度发布
fix(agent): 修复日志轮转后 inode 未重新打开导致的漏采
```

---

## 4. 错误处理与日志

### 4.1 错误必须 wrap，不许吞

```go
// ❌ 错误示范
if err != nil { return err }
_ = doSomething()

// ✅ 正确
if err != nil {
    return fmt.Errorf("load config %s: %w", path, err)
}
```

**自定义错误类型**（`internal/pkg/apperr`）：

```go
type Code string
const (
    CodeNotFound      Code = "NOT_FOUND"
    CodeInvalid       Code = "INVALID_ARGUMENT"
    CodeConflict      Code = "CONFLICT"
    CodePrecondition  Code = "FAILED_PRECONDITION"  // 如：nginx -t 失败
    CodeUnavailable   Code = "UNAVAILABLE"          // 如：Agent 离线
    CodeInternal      Code = "INTERNAL"
)

type Error struct {
    Code    Code
    Message string        // 面向用户的中文消息
    Detail  string        // 面向开发者的技术细节
    Cause   error
}
```

**API 响应统一格式**：

```jsonc
// 成功（列表）
{ "code": 0, "data": { "items": [...], "total": 42 } }
// 成功（单对象）
{ "code": 0, "data": { "id": 1, "name": "..." } }
// 失败
{ "code": 4001, "message": "配置语法错误", "detail": "nginx: [emerg] unknown directive \"lstne\" in /etc/nginx/conf.d/api.conf:12" }
```

### 4.2 日志（zerolog 结构化）

```go
log.Info().
    Str("node", node.Name).
    Int("change_id", order.ID).
    Msg("config deployed")
```

**日志级别约定**：
- `Debug` —— 开发调试，生产关闭
- `Info` —— 关键业务动作（发布开始/结束、证书续期、节点上下线）
- `Warn` —— 可自愈的异常（Agent 断连重连、校验失败被拦下）
- `Error` —— 需要人介入（发布失败、回滚触发、数据库不可用）

**必须带上下文的字段**：`node_id` / `change_id` / `task_id` / `trace_id`。日志要能串起来。

**禁止**：
- ❌ 日志里打印私钥、证书内容、Token、密码
- ❌ 用 `fmt.Println` 打日志

---

## 5. 数据库约定

### 5.1 用 ent，不裸写 SQL

```bash
# 修改 ent/schema/*.go 后必须跑
go generate ./ent
make migrate-dev      # 生成并应用迁移到开发库
```

**复杂查询**（聚合、窗口函数、递归 CTE）写在 `internal/repo/` 下的独立文件，用 ent 的 `sql/modifier` 或原生 `*sql.DB`，并加注释说明为什么 ent DSL 表达不了。

### 5.2 schema 约定

**每个实体必须有**：

```go
func (Node) Fields() []ent.Field {
    return []ent.Field{
        field.Time("created_at").Default(time.Now).Immutable(),
        field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
    }
}
```

**软删除**：配置、节点等实体用 `field.Time("deleted_at").Optional()`，不做物理删除（审计要求）。

**枚举用 Go 常量 + ent 的 `field.Enum`**，值用小写下划线（`real_server` / `director`），在 API 层转成前端友好的形式。

### 5.3 核心实体关系速查

```
Node ──< NodeCapability          （能力基线快照，每次采集一行，不覆盖）
Node ──< ConfigSnapshot          （发布前快照）
Node ──< DeployTask              （单节点发布任务）

ConfigFile ──< ConfigRevision    （版本链，parent_id 串联）
ConfigRevision >── ConfigBlob    （内容寻址，SHA256 去重）

ChangeOrder ──< DeployTask       （一次变更 = N 个节点任务）
ChangeOrder >── ConfigRevision   （变更的目标版本）

Certificate ──< CertIssueLog

LVSCluster ──< VirtualService ──< RealServer >── Node
                                     └── mode: active-standby | ecmp   ← 预留扩展

SecurityRule ──< SecurityEvent
SecurityEvent >── ChangeOrder    （封禁走发布流水线）

BuildArtifact ──< UpgradeTask >── ChangeOrder

AuditLog                          （append-only，禁止 UPDATE/DELETE）
```

### 5.4 开发态用 SQLite

```bash
# 开发（零依赖）
NGXCP_DB_DRIVER=sqlite NGXCP_DB_DSN="file:./dev.db?cache=shared&_fk=1"

# 生产
NGXCP_DB_DRIVER=postgres NGXCP_DB_DSN="postgres://ngxcp:pwd@127.0.0.1:5432/ngxcp?sslmode=disable"
```

**方言差异规避**：不写数据库特有语法（不用 PG 的 `ON CONFLICT`、不用 SQLite 的 `INSERT OR REPLACE`）；需要 upsert 时用 ent 生成的 API。**每完成一个数据库相关任务，必须同时验证 SQLite 和 PG 两条路径。**

---

## 6. Agent 安全红线（不可协商）

**Agent 是被部署到生产 nginx 机器上的高权限程序。以下约束是硬性的：**

1. **❌ 绝不提供任意命令执行接口** —— 没有 `ExecCommand(cmd string)` 这种 RPC。所有操作是**预定义指令**（`SyncConfig` / `Reload` / `SetWeight` / `UpgradeBinary` ...），参数强类型、范围受限
2. **✅ 路径白名单** —— Agent 只能读写 `/etc/nginx/**`、`/var/lib/ngxcp/**`、`/var/log/nginx/**`。越界请求直接拒绝
3. **✅ 指令签名** —— 控制面下发的每条指令带 HMAC 签名，Agent 验签后才执行
4. **✅ mTLS 双向认证** —— Agent 主动外连控制面（出向），节点不开放任何入站端口
5. **✅ 指令幂等** —— 同一 `task_id` 重复下发只执行一次
6. **✅ 超时与并发控制** —— 单指令默认 60s 超时；同一节点串行执行，不并发

**Agent 不得做**：
- ❌ 在日志或错误里输出私钥内容
- ❌ 自动执行未签名的指令
- ❌ 写入白名单外的路径（即使是控制面要求的）

---

## 7. 测试要求

### 7.1 测试分层

| 类型 | 覆盖对象 | 工具 | 要求 |
| --- | --- | --- | --- |
| **单元测试** | 纯函数：配置解析、diff、校验规则、模板渲染 | `testing` + `testify` | 核心逻辑覆盖率 ≥ 80% |
| **集成测试** | 数据库、ClickHouse、gRPC | `testcontainers-go` | 每个 repository 至少 happy path + 一个错误分支 |
| **端到端测试** | 完整发布流程 | Docker Compose 起 2 个 nginx 容器 | M3 之后必须有一个"发错误配置 → 零污染 → 自动回滚"的用例 |

### 7.2 AI 必须自测

**每个任务完成后，AI 必须实际执行验收命令并输出结果。不许说"应该没问题"。**

```bash
make test                  # 全量测试
make test PKG=./internal/domain/config/...   # 单包
make lint                  # golangci-lint
make build                 # 编译控制面 + Agent
make e2e                   # 端到端（需 Docker）
```

### 7.3 测试数据

- 用 `testdata/` 目录放 fixture（真实 nginx 配置片段、nginx -V 输出样例）
- **必须包含用户真实环境的样例**：`testdata/nginx_V_1.30.0.txt`（用户提供的 `-V` 输出）
- 不 mock 数据库，用 testcontainers 起真 PG

---

## 8. AI 工作流规则 ★ 最重要的一节

### 8.1 会话开始清单

```
① 读 AGENTS.md（本文件）
② 读 docs/tasks/README.md（任务总览与工作流）
③ 读 docs/tasks/M{N}-{name}.md（当前里程碑全部任务）
④ 读即将修改的文件（让 AI 看到现状，不许它猜）
⑤ 按需读 docs/ARCHITECTURE.md 的相关章节（不要全读，太长会稀释注意力）
```

### 8.2 任务执行流程

```
1. 复述任务目标（一句话，确认理解一致）
2. 列出要创建/修改的文件清单
3. 读现有代码（如有）
4. 实现
5. 跑验收命令，贴出真实输出
6. 更新任务文件里的状态标记 [ ] → [x]
7. 总结：做了什么、验证了什么、有什么遗留
```

### 8.3 粒度控制

| 信号 | 动作 |
| --- | --- |
| 一个任务要改 > 8 个文件 | **停下，拆分任务** |
| 单个文件 > 400 行 | **停下，按职责拆分** |
| 单个函数 > 60 行 | 提取子函数 |
| 发现自己在不确定的设计上做选择 | **停下问用户**，不要猜 |
| 发现架构文档与实现冲突 | **停下报告**，不要自己改架构 |

### 8.4 四条铁律

1. **不许改架构** —— 技术栈、模块边界、数据模型由 `docs/ARCHITECTURE.md` 和 `ent/schema/` 定义。有疑问就问，不擅自发挥
2. **不许裸写 SQL** —— 用 ent；确需复杂查询，写在 `internal/repo/` 并加注释
3. **不许吞错误** —— 所有 error 必须处理或 wrap；不许 `_ =`
4. **不许跳过验证** —— 验收命令必须实际执行并贴输出

### 8.5 上下文管理

- 不要把三份大文档（PRD / ARCHITECTURE / DECISIONS，合计 2900+ 行）一次性全塞进上下文
- **按需引用章节**：`请阅读 docs/DECISIONS.md 的决策 13（Agent 能力发现）`
- 每个里程碑结束，**让用户新开一个会话**继续下一个里程碑（避免上下文累积导致质量下降）

### 8.6 给 AI 的提示词模板

```
角色：你是 NGX-CP 项目的开发工程师（Go 全栈）。

背景：NGX-CP 是管理 2 Keepalived(Director) + 2 Nginx(RealServer, LVS-DR) 的配置平台。
核心是把配置变更做成"可校验→可灰度→可观测→可回滚"的流水线。

请先阅读：
1. /Users/tianhao/git/Nginx-Cluster-Manager/AGENTS.md（项目宪法，必读）
2. /Users/tianhao/git/Nginx-Cluster-Manager/docs/tasks/M1-skeleton.md

然后执行任务 T013（Agent 心跳与会话管理）。

要求：
- 严格按任务文件里的"涉及文件"与"接口契约"实现
- 完成后执行验收命令并贴出真实输出
- 如果发现问题或需要决策，先说明再停下，不要自行发挥
```

---

## 9. 陷阱清单（踩过的和必然会踩的）

### 9.1 nginx 相关

| 陷阱 | 说明 |
| --- | --- |
| **`nginx -t` 要用 staging 目录的完整配置测** | 单独测一个 conf 文件会因为 include 相对路径而误判。必须 `-p <prefix> -c <conf>` 在完整上下文里测 |
| **staging 必须与 `/etc/nginx` 同分区** | 否则 `rename` 不原子。Agent 上线自检必须检查 |
| **reload 失败不会让 nginx 停止服务** | 老进程继续跑，新配置不生效。**必须靠探活确认，不能只看 reload 返回码** |
| **`nginx -T` 在配置有语法错误时会失败** | 这不是 bug 是特性 —— 用它检出错配置。但要区分"文件不存在"和"语法错误" |
| **`--add-module=../xxx` 的路径是编译机的** | 运行时无意义，从路径末段提取模块名 |
| **reload 期间新旧 worker 并存** | 探活窗口要覆盖 reload 时间，默认等 5s 再开始探活 |
| **DR 模式下 `nginx -t` 通过不代表业务可用** | 还要检查 VIP 是否绑在 lo:0、ARP 是否抑制（决策 4） |
| **upstream check module 的 patch 与 nginx 版本强绑定** | 选错 patch 会编译成功但运行异常，必须查 `configs/module_matrix.json` |

### 9.2 LVS / Keepalived

| 陷阱 | 说明 |
| --- | --- |
| **云厂商 VPC 禁 VRRP 组播** | 必须配 `unicast_src_ip` + `unicast_peer`。用户是自建物理机，但代码要支持 |
| **DR 要求 Director 与 RS 同二层** | 跨机房/跨可用区做不了 DR，要改 TUN 或 NAT |
| **DR 不支持端口映射** | VIP:80 必须对应 RS:80，端口不一致要校验拦截 |
| **`arp_ignore`/`arp_announce` 漂移是最难排查的故障** | 表现为流量时通时断。必须每 5 分钟巡检 |
| **`rp_filter` 严格模式会丢包** | DR 回包源 IP 是 VIP，严格 RPF 会判定为伪造包丢弃 |
| **Keepalived 2.x 移除了 AH 认证** | 只剩 `auth_type PASS` 明文，安全靠网络隔离 |
| **摘除 RS（w=0）不会断开已有连接** | 必须等排空（`ipvsadm -Ln --stats` 看 ActiveConn 归零） |
| **脑裂检测** | 两台 Director 同时上报持有 VIP → 立即 CRITICAL |

### 9.3 日志与采集

| 陷阱 | 说明 |
| --- | --- |
| **logrotate 后 inode 变化** | 必须监控 inode，变化则重新打开；`copytruncate` 模式要检测文件大小骤降 |
| **轮转瞬间的日志丢失** | 轮转后立即扫旧文件尾部 |
| **多节点时间不同步会让检索失效** | 检查 NTP 同步状态，时间偏差 > 1s 告警 |
| **`access_log off;` 和 `syslog:` 要跳过** | 前者无文件，后者是远端，都不采集 |
| **ClickHouse 批量写入要攒批** | 单条插入性能极差，用 async_insert + 批量（1000 条 / 5s） |
| **ClickHouse 必须设 `max_memory_usage`** | 默认会吃系统内存 90%，设 6G |

### 9.4 Go / 工程

| 陷阱 | 说明 |
| --- | --- |
| **Agent 静态编译要 `CGO_ENABLED=0`** | 否则目标机 glibc 版本不匹配跑不起来 |
| **交叉编译要注意 GOOS/GOARCH** | 目标机是 linux/amd64 |
| **gRPC 流要处理重连** | 指数退避，最大 60s；断连期间指令要能排队 |
| **goroutine 泄漏** | 所有 `go func()` 必须有退出条件或 ctx 取消 |
| **文件描述符耗尽** | 日志采集多文件时注意，设 ulimit 检查 |
| **SQLite 并发写锁** | 开发态遇到 `database is locked` 时，检查是否忘了开 WAL 或事务过长 |
| **时间统一用 UTC 存储，展示时转本地** | 日志检索跨时区会混乱 |

### 9.6 虚拟化与网络（vSphere / 万兆 LACP）★ 环境特有

运行环境：**2 台 TH-D2110 做 ESXi 虚拟化（vCenter 管理），全万兆，VDS 配 LAG/LACP。**

| 陷阱 | 说明 |
| --- | --- |
| **Director 端口组必须开「伪传输」** | LVS-DR 改目标 MAC 后源 MAC 仍是上游的 → 被判伪造传输 → vSwitch 静默丢包。**症状极具误导性：`ipvsadm` 转发计数正常增长，但 RS 访问日志一条没有** |
| **Keepalived VRRP 需要「MAC 地址更改」+「混杂模式」** | VRRP 用虚拟 MAC `00:00:5E:00:01:<VRID>`。不开 → 双机都认自己 Master → **脑裂** |
| **这些策略只能在 vCenter 配，Agent 检测不到** | 发生在 hypervisor 层。必须做成部署检查清单的强制项，不能指望运行时自检 |
| **混杂模式不要全局开** | 有 CPU 开销 + 同网段流量可被嗅探。Director 单独端口组，只在那一个组开 |
| **LACP 默认算法（源虚拟端口）只用到一条 10G** | 必须改为**基于 IP 哈希**，否则万兆 LAG 白配 |
| **单条 TCP 流上限 = 单物理链路 10G** | LAG 不叠加单流带宽。大文件下载场景要注意 |
| **LAG 成员必须终结于同一交换机** | 跨独立交换机（未堆叠/未 MLAG）做不了 LACP，应改 LBT |
| **e1000 网卡在万兆下 CPU 打满** | 必须 **vmxnet3 + RSS 多队列**（队列数 = vCPU 数） |
| **LRO 在 Director 上建议关闭** | 会合并 TCP 段，可能干扰 LVS 包处理；TSO/GRO 在 RS 上可开 |
| **VM 时钟漂移比物理机严重** | 必须 chrony，**并关闭 VMware Tools 时间同步**（两者会互相纠正导致时钟跳变，直接破坏日志时序）。偏差 > 1s 告警 |
| **vMotion 会引发误告警** | Director vMotion 期间有几百毫秒中断，且可能触发脑裂误报。脑裂检测容忍窗口要 > 3s；LVS check 参数 `fall=3` 容忍抖动 |
| **DRS 可能把主备调度到同一物理主机** | 必须配**反亲和性规则**：director-01 ⊥ director-02，rs-01 ⊥ rs-02 |
| **"按需分配"会让关键 VM 被挤压** | Director 设**全部资源预留**（2C/4G 绝对量小，成本极低）；RS 预留 50% |
| **vCPU > 56 才需考虑 vNUMA** | TH-D2110 单 NUMA 节点 = 28 核 56 线程。nginx VM 给 8 vCPU 天然 fit 单节点 |

### 9.5 前端

| 陷阱 | 说明 |
| --- | --- |
| **Monaco 体积大** | 用 `vite` 的 code splitting，编辑器页面懒加载 |
| **SSE/WebSocket 重连** | 发布进度用 SSE，断连要自动重连并恢复进度 |
| **大配置 diff 会卡** | 超过 5000 行的 diff 用虚拟滚动或截断 |
| **Grafana iframe 要 Auth Proxy** | 见 DECISIONS §11.5，配 `X-WEBAUTH-USER` header |

---

## 10. 环境与命令

### 10.1 首次搭建

```bash
# 1. 起中间件
docker compose up -d

# 2. 生成 ent 代码
go generate ./ent

# 3. 应用数据库迁移
make migrate-dev

# 4. 生成 protobuf（需 protoc + protoc-gen-go + protoc-gen-go-grpc）
make proto

# 5. 启动控制面（开发模式，内置 mock Agent）
make dev-server

# 6. 启动前端
cd web && pnpm install && pnpm dev
```

### 10.2 常用命令（Makefile）

```makefile
make dev           # 起全套依赖 + 控制面 + 前端
make build         # 编译控制面 + Agent（静态）
make test          # 全量测试
make lint          # golangci-lint
make fmt           # gofmt + goimports
make proto         # 生成 protobuf
make ent           # 生成 ent 代码
make migrate-dev   # 应用迁移到开发库
make e2e           # 端到端测试
make backup        # 手动触发备份
make clean         # 清理产物
```

### 10.3 关键环境变量

```bash
NGXCP_DB_DRIVER=postgres
NGXCP_DB_DSN="postgres://ngxcp:pwd@127.0.0.1:5432/ngxcp?sslmode=disable"
NGXCP_LISTEN=:8080
NGXCP_AGENT_GRPC=:9443
NGXCP_MASTER_KEY_FILE=/etc/ngxcp/master.key       # 加密 CF token / 私钥的主密钥
NGXCP_CA_CERT=/etc/ngxcp/pki/ca.crt
NGXCP_CA_KEY=/etc/ngxcp/pki/ca.key
NGXCP_CLICKHOUSE=http://127.0.0.1:8123
NGXCP_VM_WRITE=http://127.0.0.1:8428
NGXCP_LOG_LEVEL=info
```

---

## 11. 核心不变量（任何改动都不得破坏）

这些是项目的"物理定律"，代码评审时优先检查：

1. **线上配置永不被未经校验的内容污染** —— 任何写入必须经过 `nginx -t`
2. **任何变更可回滚** —— 发布前必须有快照；封禁、证书续期也走变更单
3. **Agent 不提供任意命令执行** —— 见 §6
4. **私钥永不出控制面边界的加密存储，永不下发到浏览器**
5. **节点上从不需要安装 Git / rsync / Python** —— Agent 是零依赖单二进制
6. **DR 合规不通过的节点不得参与 LVS 发布**
7. **审计日志 append-only** —— 禁 UPDATE / DELETE
8. **同分区 rename 才是原子落盘** —— 不同分区必须降级并告警
9. **Director 的端口组安全策略（混杂/MAC更改/伪传输）是部署前置条件** —— 平台只能提示清单，无法运行时修复
10. **所有节点时间必须同步（偏差 < 1s）** —— 否则跨节点日志检索与 TraceID 追踪失去意义

---

## 12. 参考资料

| 文档 | 用途 |
| --- | --- |
| `docs/PRD.md` | 需求、功能优先级、数据模型概述、里程碑 |
| `docs/ARCHITECTURE.md` | 技术架构、模块设计、API 清单、安全设计 |
| `docs/DECISIONS.md` | **全部架构决策与论证**（15 个决策）。重要：技术选型有疑问先查这里 |
| `docs/tasks/` | AI 任务清单，分里程碑 |
| `prototype/index.html` | 高保真交互原型（单文件，浏览器直接打开）。**前端实现的视觉与交互基准** |
| `configs/module_matrix.json` | nginx 版本 × 模块补丁兼容矩阵 |

**决策快速索引**：

| 问题 | 查 DECISIONS 哪一节 |
| --- | --- |
| 为什么不用 scp / rsync | §1 |
| 配置为什么存数据库不存 Git | §2 |
| 日志怎么统一、怎么防攻击 | §3 |
| LVS-DR 有哪些硬约束 | §4 |
| 证书怎么签发分发 | §5 |
| nginx 1.30 的能力基线 | §6 |
| 权限怎么做 | §7 |
| 百万级访问够不够用 | §8 |
| 为什么用 PG 不用 SQLite | §9 |
| 要不要时序数据库 | §10 |
| 监控用 Grafana 还是自研 | §11 |
| nginx 怎么编译升级 | §12 |
| Agent 怎么发现配置与日志目录 | §13 |
| 部署拓扑与资源预算 | §14 |
| AI 编程怎么组织 | §15 |
| vSphere / 万兆 LACP 环境注意事项 | §16 |

---

## 13. 变更记录

| 日期 | 变更 |
| --- | --- |
| 2026-09-03 | 初版。基于 DECISIONS 第一轮 + 第二轮决策 |
| | 第二轮修订：SQLite → PostgreSQL；新增监控子系统与构建升级中心；补充容量评估与资源预算 |
| | 环境更新：2 台 TH-D2110 做 ESXi 虚拟化（vCenter），全万兆 + VDS LAG/LACP。新增 §9.6 虚拟化陷阱、核心不变量 9/10、决策 16 |
