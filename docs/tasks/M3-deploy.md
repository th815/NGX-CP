# M3 · 发布引擎（W5–W6）★ 核心里程碑

> **目标**：把配置变更变成一条可灰度、可观测、可回滚的流水线。
> **完成标志**：故意下发语法错误的配置 → **零节点被污染** → 自动回滚 → 有审计记录。
>
> **这是整个项目最关键的里程碑。** 前面所有能力都是为它准备的。

---

## T030 · 变更单模型与状态机 ✅ 已完成（2026-09-04）

**目标**：定义"一次变更"的完整生命周期。

**依赖**：T021, T006

**涉及文件**：
```
ent/schema/change_order.go
ent/schema/deploy_task.go
internal/domain/deploy/order.go
internal/domain/deploy/state.go
internal/domain/deploy/state_test.go
internal/server/handler/deploy.go
```

**契约**：

```go
type ChangeOrder struct {
    ID          int
    Title       string
    Type        string    // "config" | "cert_renew" | "security_block" | "lvs" | "upgrade" | "rollback"
    Source      string    // "manual" | "api" | "schedule" | "auto_renew"
    Status      string    // 见状态机
    TargetNodes []int     // [1,2]
    ConfigRevisionIDs []int
    Strategy    DeployStrategy
    SnapshotID  *int
    CreatedBy   string
    ApprovedBy  *string
    CreatedAt, StartedAt, FinishedAt time.Time
    Comment     string
}

type DeployStrategy struct {
    Mode            string   // "serial" | "batch" | "all_at_once" | "lvs_graceful"  ★
    BatchSize       int      // serial 时忽略
    ObserveWindow   int      // 秒，每批之后的观测时长
    FailureThreshold float64 // 失败率阈值，默认 0（任一失败即熔断）
    AutoRollback    bool     // 默认 true
    ApprovalRequired bool
}

type DeployTask struct {         // 单节点任务
    ID          int
    ChangeOrderID int
    NodeID      int
    Status      string
    Steps       []TaskStep       // 9 步执行序列
    CurrentStep int
    Error       string
    StartedAt, FinishedAt time.Time
}

type TaskStep struct {
    Name   string    // "transfer" | "validate" | "snapshot" | "switch" | "reload" | "probe"
    Status string    // "pending" | "running" | "success" | "failed" | "skipped"
    StartedAt, FinishedAt time.Time
    Output string
}
```

**状态机**（控制面侧）：

```
draft ──提交──> pending_approval ──批准──> pending
                      │                      │
                      └──拒绝──> rejected    │
                                             ▼
                                          running ──┬──> success
                                             │      │
                                             │      ├──> failed ──> rolling_back ──┬──> rolled_back
                                             │      │                              └──> rollback_failed ★
                                             │      └──> partial_success
                                             ▼
                                         canceled
```

**状态转换必须持久化**（数据库事务 + 乐观锁），不能只放内存 —— 控制面重启后要能恢复。

```go
func (s *Service) Transition(ctx context.Context, orderID int, from, to string) error {
    // UPDATE change_orders SET status=? WHERE id=? AND status=?
    // 影响行数 0 → 并发冲突，返回错误
}
```

**验收命令**：
```bash
go generate ./ent && make migrate-dev
go test ./internal/domain/deploy/... -run TestStateMachine -v
# 覆盖：
#   - 合法转换成功
#   - 非法转换（success → running）被拒绝
#   - 并发转换只有一个成功（乐观锁）
#   - 控制面重启后能恢复 running 状态的任务

go test ./internal/domain/deploy/... -run TestTransition -race -v   # 竞态检测
```

**陷阱** ⚠️：
- ⚠️ **必须用 `-race` 跑并发测试**，发布引擎涉及大量 goroutine
- ⚠️ `rollback_failed` 是**最危险的状态** —— 意味着节点处于未知状态。必须**立即 CRITICAL 告警 + 停止所有其他变更 + 人工介入**
- ⚠️ 状态转换要用数据库事务，不要靠内存标志位

---

## T031 · 发布前快照 ✅ 已完成（2026-09-04）

**目标**：变更前保存可恢复的完整状态。

**依赖**：T030, T021

**涉及文件**：
```
internal/domain/backup/snapshot.go        # 领域模型 + 保留策略（纯函数，已单测）
internal/agent/executor/snapshot.go        # Agent 侧 tar+gzip 执行器（含权限/属主还原）
internal/agent/executor/snapshot_test.go   # 创建/恢复/SSL 开关三测
internal/domain/backup/snapshot_test.go    # 保留策略单测
proto/agent/v1/agent.proto                 # 追加 CREATE_SNAPSHOT / RESTORE_SNAPSHOT 命令 + 消息
```

**架构决策 ⚠️（与原任务注释的差异）**：原任务注释写「追加 `CreateSnapshot`/`RestoreSnapshot` RPC」。
但本项目 Agent 是**主动外连**、节点**不开放入站端口**，控制面无法主动调用 Agent 的 RPC；
且现有 T024 校验已确立「命令经 Heartbeat 双向流下发」的同构模式。因此 T031 复用该模式：
在 `HeartbeatResponse.Command` 增加 `CREATE_SNAPSHOT=4` / `RESTORE_SNAPSHOT=5`，
分别携带 `SnapshotCreateTask` / `SnapshotRestoreTask`，结果经 `HeartbeatRequest.SnapshotResult` 回传。
`gen/agent/v1` 代码因本机未装 `protoc` 尚未重新生成——proto 文本即契约事实来源，
**控制面下发命令 / Agent 接线的端到端集成推到 T032（原子落盘）一并落地**。

**契约**：

```go
type ConfigSnapshot struct {
    ID        int
    NodeID    int
    ChangeOrderID *int
    Type      string     // "pre_deploy" | "manual" | "scheduled"
    Path      string     // /var/lib/ngxcp/snapshots/<node>/<ts>.tar.gz
    Files     []SnapshotFile
    Size      int64
    CreatedAt time.Time
}

type SnapshotFile struct {
    Path   string
    SHA256 string
    Size   int64
    Mode   os.FileMode
    Owner  string     // nginx:nginx
}
```

**Agent 侧执行**：

```bash
# 快照内容：不只是配置文件！
tar czf /var/lib/ngxcp/staging/snapshot-<ts>.tar.gz \
    -C / --exclude='*.log' \
    etc/nginx/ \
    etc/keepalived/ \            # Director 角色
    etc/nginx/ssl/ \             # 证书（可选，单独开关）
```

**必须记录文件元数据**（mode / owner），恢复时才能还原权限。

**保留策略**：
```yaml
snapshot:
  keep_days: 90
  keep_max_per_node: 200
  include_ssl: false        # 证书有独立管理，默认不打进快照
```

**验收命令**：
```bash
go test ./internal/agent/executor/... -run TestSnapshot -v

# 真机验证
curl -s -X POST http://localhost:8080/api/v1/snapshots -d '{"node_id":1,"type":"manual"}' | jq
ssh rs-nginx-01 "ls -la /var/lib/ngxcp/snapshots/1/"
# 期望：有一个 tar.gz，解压后能看到 etc/nginx/ 目录结构

# 权限还原测试
ssh rs-nginx-01 "chmod 600 /etc/nginx/nginx.conf"
# 恢复快照后检查权限是否还原为 644
```

**陷阱** ⚠️：
- ⚠️ **快照必须保存文件权限与属主** —— 只存内容会导致恢复后 nginx 读不了配置
- ⚠️ **快照里默认不含证书私钥**（`include_ssl: false`）—— 私钥有独立生命周期，混进快照会增加泄露面
- ⚠️ 快照 tar 要在** staging 目录生成**再移动，避免生成到一半被误用
- ⚠️ 磁盘清理：超过保留策略的快照要有 worker 定期清理，否则写满磁盘

---

## T032 · Agent 原子落盘执行器 ★ ✅ 已完成（2026-09-04）

**目标**：实现"要么完全生效，要么完全不变"的配置写入。

**依赖**：T031, T024

**涉及文件**：
```
internal/agent/executor/deploy.go          # 9 步流水线编排（Deploy + atomicSwitch + restoreAndReload）
internal/agent/executor/deploy_test.go      # happy/语法错误零污染/探活失败回滚/摘要不符/无探活/观测窗口
internal/pkg/atomicfile/atomic.go           # 原子移动（同盘 rename / 跨盘 copy 降级）
internal/pkg/atomicfile/atomic_test.go
proto/agent/v1/agent.proto                  # 追加 DEPLOY_CONFIG 命令 + SyncConfigTask/DeployProgress（Heartbeat 通道）
```

**实现说明（与原任务注释的偏差）**：
- 原注释列 `internal/pkg/shellx/exec.go` 为新建包。但 reload 命令经既有 `hostexec.CommandExecutor.Output` 同一抽象执行（T024 已用），**不再单独建 shellx 包**以免重复——`DeployExecutor` 直接持有 `CommandRunner`（与 `validate.Executor` 同接口）。
- ctrl 面触发方式：原注释写「`rpc SyncConfig`」。与 T031 同源，Agent 主动外连无入站端口，`SyncConfig` 复用 **Heartbeat `DEPLOY_CONFIG=6` 命令**下发 `SyncConfigTask`，进度经 `HeartbeatRequest.DeployProgress` 回传（gen 代码待 protoc，端到端接线随 M3 集成验收）。
- 步骤④ 直接复用 T031 的 `SnapshotExecutor`；步骤⑧ 探活走可注入的 `Prober` 接口（默认 `HTTPProber`，T033 将替换为复合探活）。

**验收命令**：
```bash
go test ./internal/agent/executor/... -run TestDeploy -v
go test ./internal/pkg/atomicfile/... -v
go test ./internal/agent/executor/... -race -v   # 流水线无竞态（progress 非阻塞发送）
```
★ 最关键的端到端验证（docker 起真实 nginx）推到 M3 集成验收（`make e2e`）。

**契约 —— 9 步原子执行序列**（这是整个项目的核心算法）：

```
① 传输        → 把配置内容写入 staging 目录（与 /etc/nginx 同分区）
② 校验摘要    → SHA256 比对，不符立即中止
③ 语法校验    → nginx -t -p <prefix> -c <staging>/nginx.conf
                 ↓ 失败 → 删除 staging，线上一个字节都没动，返回失败
④ 创建快照    → tar 打包 /etc/nginx（保留权限与属主）
⑤ 原子切换    → 逐个 rename 到目标位置（同分区 rename 是原子操作）
⑥ 平滑加载    → nginx -s reload（或 systemctl reload）
⑦ 等待        → sleep 5s（reload 期间新旧 worker 并存）
⑧ 双层探活    → 本地 curl + 控制面远程探活
                 ↓ 失败 → 从步骤 ④ 的快照恢复 → reload → 返回失败（触发回滚）
⑨ 上报结果    → 成功，更新节点当前版本
```

```go
func (e *Executor) Deploy(ctx context.Context, req *SyncConfigRequest, progress chan<- Progress) error {
    steps := []Step{
        {"transfer", e.transfer},
        {"verify_hash", e.verifyHash},
        {"validate", e.validate},      // ★ 校验在切换之前
        {"snapshot", e.snapshot},
        {"switch", e.atomicSwitch},
        {"reload", e.reload},
        {"wait", e.waitStabilize},
        {"probe", e.probe},
        {"report", e.report},
    }
    // 任何一步失败 → 已执行的步骤逆序补偿
}
```

**原子切换的实现**：

```go
func (e *Executor) atomicSwitch(files []FileToWrite) error {
    // 检查同设备（T018 的结果）
    if !e.atomicWriteCheck.SameDevice {
        return e.fallbackCopySwitch(files)   // 降级：copy + 校验 + 文件锁
    }
    for _, f := range files {
        staging := filepath.Join(e.stagingDir, f.Path)
        target := filepath.Join(e.prefix, f.Path)
        os.MkdirAll(filepath.Dir(target), 0755)
        if err := os.Rename(staging, target); err != nil {   // ★ 原子
            return fmt.Errorf("rename %s: %w", f.Path, err)
        }
    }
}
```

**验收命令**：
```bash
go test ./internal/agent/executor/... -run TestDeploy -v
# 用例（用 docker 起真实 nginx 容器做集成测试）：
#   ✅ 正常配置 → 9 步全成功，配置生效，nginx 正常
#   ✅ 语法错误配置 → 步骤 ③ 失败，节点配置零变化，staging 已清理
#   ✅ 探活失败 → 步骤 ⑧ 失败，自动从快照恢复，nginx 仍在服务
#   ✅ 传输中断 → 步骤 ② 摘要不符，中止
#   ✅ 同分区 & 跨分区两种路径都要覆盖

go test ./internal/agent/executor/... -race -v

# ★ 最关键的端到端验证
make e2e
# 期望：下发错误配置 → 零节点被污染 → 自动回滚 → 审计记录完整
```

**陷阱** ★⚠️：
- **校验必须在切换之前**（步骤 ③ 在 ⑤ 之前）—— 这是"零污染"的根本保证。顺序错了整个设计失效
- **reload 成功不代表配置生效** —— 老 worker 继续服务。必须探活（步骤 ⑧）
- **探活失败必须自动从快照恢复**，不能只标记失败等人工处理
- staging 目录要与 `/etc/nginx` **同分区**，否则 rename 不原子（T018 已检查，这里要用其结果）
- 步骤 ⑤ 如果中途失败（第 3 个文件 rename 失败），**前 2 个已经生效了** —— 这时必须靠步骤 ④ 的快照恢复
- 每个步骤要有**超时**（默认 60s），不能无限等待

---

## T033 · 探活器 ✅ 已完成（2026-09-04）

**目标**：判断变更后的节点是否真的健康。

**依赖**：T032

**涉及文件**：
```
internal/agent/probe/probe.go          # 核心类型（ProbeType/ProbeConfig/ProbeResult/Prober）+ New 工厂
internal/agent/probe/http.go            # HTTPProbe（GET URL，<500 或 ExpectCode 即健康）
internal/agent/probe/tcp.go             # TCPProbe（端口连通）
internal/agent/probe/log.go             # LogErrorProbe（reload 后观测窗口内错误日志增量）
internal/agent/probe/composite.go       # CompositeProbe（全部通过才健康，AND 语义）
internal/agent/probe/probe_test.go      # HTTP/TCP/日志/复合 11 例
internal/agent/executor/probe_adapter.go # probe.Prober → executor.Prober 适配器 + SetProbeConfigs
internal/agent/executor/deploy.go        # 删内置 HTTPProber，默认经 adapter 构造 HTTP 探活
```

**实现说明（与原任务注释的偏差）**：
- 复合探活独立成 `internal/agent/probe` 包（不放在 executor 包内），executor 内旧的 `HTTPProber` 已删除以避免重复实现；executor 经 `probeAdapter` 接入 `DeployExecutor.SetProber`/`SetProbeConfigs`，原有 `Prober` 接口签名不变（保证 T032 的 `fakeProber` 测试契约稳定）。
- `external` 探活复用 `HTTPProbe`（访问 VIP/外部端点），`tcp` 用 `TCPProbe`，`log_error` 用 `LogErrorProbe`（默认窗口 30s、上限 3 条；reload 之后才记录 offset，避免把历史错误算入）。
- 探活器统一 `Probe(ctx) (*ProbeResult, error)`，受 `context.WithTimeout` 约束（陷阱：不用 http.Client 默认无超时），连接超时在 Timeout 内返回不卡死。
- 端到端接线（Agent 经 Heartbeat 命令触发探活、控制面从外部探活 VIP）随 M3 集成验收（T037 SSE + 集成）一并落地。


**契约**：

```go
type ProbeType string
const (
    ProbeHTTP     ProbeType = "http"        // 本地 HTTP 探活
    ProbeTCP      ProbeType = "tcp"         // 端口连通
    ProbeLogError ProbeType = "log_error"   // ★ 错误日志增量
    ProbeExternal ProbeType = "external"    // ★ 控制面从外部探活 VIP
)

type ProbeConfig struct {
    Type        ProbeType
    URL         string        // http://127.0.0.1/healthz
    ExpectCode  int           // 200
    Timeout     time.Duration
    Retries     int
    // log_error 专用
    LogPath     string
    ErrorPattern string       // 正则，匹配 error.log 里的错误
    MaxNewErrors int          // 观测窗口内允许的新增错误数
}

type ProbeResult struct {
    OK        bool
    Detail    string
    Latency   time.Duration
    CheckedAt time.Time
}

// 双层探活：本地 + 远程
func (e *Executor) probe(ctx context.Context, cfg []ProbeConfig) (*ProbeResult, error)
```

**双层探活的必要性**（★ 这是关键设计）：

| 层 | 位置 | 检查什么 | 为什么需要 |
| --- | --- | --- | --- |
| **本地探活** | Agent 在 RS 上执行 | `curl 127.0.0.1/healthz`、`nginx -t`、error.log 增量 | 验证 nginx 进程本身健康 |
| **远程探活** | 控制面从外部访问 VIP | `curl http://VIP/healthz` | **验证端到端链路**：Director → RS → 应用 |

只用本地探活是不够的：RS 自己健康，但 LVS 可能没把它加回来、或者 ARP 配置漂移导致流量过不来。

**log_error 探活**（最实用的信号）：

```go
// 记录探活开始时的 error.log 位置，观测窗口后检查新增内容
// 新增的 emerg/alert/crit 级别日志 → 判定失败
func (p *LogProbe) Check(ctx context.Context) (*ProbeResult, error) {
    // 1. 记录当前文件大小（offset）
    // 2. 等观测窗口（如 30s）
    // 3. 读取新增部分，用 ErrorPattern 匹配
    // 4. 新增错误数 > MaxNewErrors → 失败
}
```

**验收命令**：
```bash
go test ./internal/agent/probe/... -v
# 覆盖：
#   - HTTP 200 → OK
#   - HTTP 502 → 失败
#   - 连接超时 → 失败（且能在 Timeout 内返回，不卡死）
#   - error.log 新增 emerg → 失败
#   - error.log 无新增 → OK
#   - 复合探活：全部通过才 OK

# 真机验证
curl -s -X POST http://localhost:8080/api/v1/nodes/1/probe -d '{"type":"http","url":"http://127.0.0.1/healthz"}' | jq
# 从控制面探活 VIP
curl -s -X POST http://localhost:8080/api/v1/probe/external -d '{"vip":"10.0.1.100","port":80}' | jq
```

**陷阱** ⚠️：
- ⚠️ **探活超时必须生效** —— 用 `context.WithTimeout`，不要用 http.Client 的默认（无超时）
- ⚠️ **本地探活要绕过 LVS** —— 直接访问 `127.0.0.1` 或节点自己的 IP，不要访问 VIP（否则探的是自己还是别人不确定）
- ⚠️ error.log 探活要在 reload **之后**才开始记录 offset，否则会把 reload 前的历史错误算进来
- ⚠️ `MaxNewErrors` 默认给一点余量（如 3 条），避免个别 404 误判

---

## T034 · 回滚执行器 ✅ 已完成（2026-09-04）

**目标**：失败时快速恢复到变更前的状态。

**依赖**：T032, T031, T033

**涉及文件**：
```
internal/domain/deploy/rollback.go          # 领域层：RollbackNode/RollbackChangeOrder + 状态机编排 + CRITICAL 告警接口
internal/domain/deploy/rollback_test.go     # 逆序/全成功/单败→rollback_failed+告警/不可回滚/缺快照/单节点
internal/agent/executor/rollback.go          # Agent 侧 8 步回滚执行器（复用 T031 Restore + T032 validate/reload/Prober）
internal/agent/executor/rollback_test.go     # 成功恢复/快照坏零变化/探活失败/无探活跳过/缺快照路径
proto/agent/v1/agent.proto                   # ROLLBACK_CONFIG=7 + RollbackTask（Heartbeat 通道，gen 待 protoc）
```

**实现说明（与原任务注释的偏差）**：
- 原注释列 `internal/domain/deploy/rollback.go` + `internal/agent/executor/rollback.go`（仅两份），实际补了 `rollback_test.go`（双包）与 proto 契约，领域层 `deploy.Service` 通过 `SetRollbackClient`/`SetAlertSink` 注入两个接口（`NodeRollbackClient`/`AlertSink`），不耦合传输细节——控制面经 proto `ROLLBACK_CONFIG` 命令接线，单测用 fake。
- Agent 侧回滚严格复用既有能力，未重复造轮子：`SnapshotExecutor.Restore`（T031，含权限/属主还原）、`validate.Executor`（T024/`Executor`）、`reload`、`Prober`（T033 复合探活经 `probeAdapter` 接入）。
- **回滚走完整流程且「校验在恢复之前」**：① 解压快照到 staging → ② `nginx -t` 校验快照配置（★ 快照也可能坏；此时真实 prefix 未动，直接报 `rollback_failed`）→ ③ 校验通过才 `Restore` 到真实路径 → ④ reload → ⑤ 等待 → ⑥ 探活 → ⑦ 上报。恢复后 reload/探活失败同样返回含 `Error` 的 `RollbackResult`（最危险状态）。
- `RollbackChangeOrder` 逆序回滚（最后变更的节点先回滚）；任一节点失败 → 变更单 `rollback_failed` + `AlertSink.Critical`（**节点处于未知状态，必须立即人工介入冻结变更**）；全部成功 → `rolled_back`。
- proto：`ROLLBACK_CONFIG=7` 命令 + `RollbackTask` 消息（与 T031/T032 同构走 Heartbeat 双向流），`DeployProgress.step` 已含 `rollback` 故进度复用既有通道；gen 代码待 `protoc` 安装后生成，端到端接线随 M3 集成验收。

**契约**：

```go
// 回滚一个节点到指定快照
func (s *Service) RollbackNode(ctx context.Context, nodeID int, snapshotID int) error

// 回滚整个变更单（逆序执行）
func (s *Service) RollbackChangeOrder(ctx context.Context, orderID int) error

// 回滚也要走完整流程：恢复文件 → nginx -t → reload → 探活
// ★ 回滚本身也可能失败 → rollback_failed 状态 → CRITICAL 告警
```

**回滚执行序列**（Agent 侧）：
```
① 解压快照到 staging
② nginx -t 校验快照里的配置（★ 快照配置也可能有问题）
③ 停止当前 nginx 或 reload 前先切换
④ 恢复文件（保留权限与属主）
⑤ nginx -t → 失败则中止并报 rollback_failed
⑥ reload
⑦ 探活
⑧ 上报
```

**回滚策略选择**：
```go
// 两种回滚，按场景自动选
type RollbackMode string
const (
    RollbackSnapshot  RollbackMode = "snapshot"   // 从文件快照恢复（默认，最可靠）
    RollbackRevision  RollbackMode = "revision"   // 从配置版本链恢复（更快，只改配置文件）
)

// 配置变更 → 优先用 revision（快，且版本链有完整记录）
// 证书/升级变更 → 必须用 snapshot（涉及二进制与证书文件）
```

**验收命令**：
```bash
go test ./internal/domain/deploy/... -run TestRollback -v

# ★ 端到端验证（最关键）
make e2e
# 场景：下发一个语法正确但会导致 502 的配置（如 upstream 指向不存在的端口）
# 期望：
#   1. RS1 变更 → 探活失败
#   2. 自动回滚 RS1（从快照恢复）
#   3. RS1 探活成功
#   4. 变更单标记 failed，停止继续下发 RS2
#   5. 全程业务零 5xx（因为 RS2 还在正常服务）
#   6. 审计记录完整

# 手动回滚验证
curl -s -X POST http://localhost:8080/api/v1/change-orders/15/rollback | jq
```

**陷阱** ⚠️：
- ⚠️ **回滚本身也会失败** —— 必须处理 `rollback_failed`，这是**最危险的时刻**：节点状态未知。必须立即 CRITICAL 告警 + 冻结所有变更
- ⚠️ **回滚要逆序**：多节点变更时，先回滚最后变更的节点
- ⚠️ 回滚后要**重新采集能力基线**（配置可能变了）
- ⚠️ 回滚时长目标：≤ 60 秒（单节点）

---

## T035 · LVS 权重摘除式灰度 ✅ 已完成（2026-09-04）

**目标**：让 Nginx 变更对用户完全无感。

**依赖**：T032, T033, T030

**涉及文件**：
```
internal/domain/lvs/ipvsadm.go            # VirtualServer/RealServer 类型 + ParseIPVS + VS 枚举（:80/:443tcp/:443udp）
internal/domain/lvs/graceful.go           # GracefulDeploy：DeployOne 7 步（置 0→排空→Deployer→探活→加回→观测 60s→上报），defer 必加回权重
internal/domain/lvs/lvs_test.go           # 6 例（枚举/解析/权重不变量）
internal/agent/executor/ipvs.go           # IPVSExecutor 实现 WeightSetter（hostexec 调 ipvsadm -e）
internal/domain/deploy/strategy_lvs.go    # LVSStrategy + NewLVSStrategy + SetTimings + DeployNodeCanary/DeployAll
internal/domain/deploy/strategy_lvs_test.go # 3 例（含「权重必加回」不变量）
proto/agent/v1/agent.proto                # SET_RS_WEIGHT=8 命令 + 消息（Heartbeat 通道，gen 待 protoc）
scripts/prod-lvs-probe.sh                 # 生产环境「只读」一致性探测（T035 配套，严格只读）
```

**实现说明（与原任务注释的偏差）**：
- 原注释的 `LVSGracefulDeploy` 用单 `VIP/Port/OriginalWeight` 字段 + `setWeight(nodeID, w)`，只能命中**单条 VS**。现场确认同一 RS 在 `:80`、`:443/tcp`、`:443/udp` 是**三条独立 VS**（见 `testdata/ipvsadm_Ln.txt`），只摘 `:80` 会让 443 继续打 RS、灰度失效。故改为 `BackendRef{Address}` 枚举该 backend 在**全部 VS** 上的条目统一置权（`graceful.go`），`WeightSetter` 接口按 `(vs, rs)` 逐条 `-w 0 / -w <原>`。
- `defer` 必加回权重：任何异常路径（排空超时 / 变更失败 / 探活失败）都先把节点权重复原再返回，避免把 RS 永久留池外雪上加霜（测试以「双次加回」不变量验证：正常路径 + defer 安全网）。
- 复用 T032 9 步（Deployer）+ T033 复合探活（Probers）+ T031 快照（回滚兜底），未重复造轮子。
- 生产环境只读体检（2026-09-04）确认设计契合现场（DR / wrr / persistent 60 / 3 VS），并发现 3 个问题（见 `docs/PROD-LVS-REPORT.md`）：① keepalived 组播未配 unicast_peer（违反 vSphere 铁律）；② 备机 .7 持有与 .6 完全一致的 ipvs 表 → 双 MASTER / split-brain 表象；③ 同一 RS 三条 VS，灰度须三处同步摘除。**这些均为标注建议，体检未改动任何生产配置。**
- 端到端接线（Agent 经 Heartbeat `SET_RS_WEIGHT` 命令触发、控制面从外部探活 VIP）随 M3 集成验收（T037 + T039）一并落地。

**契约 —— LVS 摘除式灰度序列**：

```
① 摘除 RS1      ipvsadm -e -t <VIP>:80 -r <RS1>:80 -w 0
② 排空连接      轮询 ActiveConn 直到 ≈ 0（或超时 120s）
③ 变更 RS1      执行 T032 的 9 步原子落盘
④ 双层探活      本地 + 控制面远程探活 VIP
⑤ 加回 RS1      ipvsadm -e -t <VIP>:80 -r <RS1>:80 -w <原权重>
⑥ 观测窗口      默认 60s：错误率、延迟、QPS 对比基线
⑦ 对 RS2 重复 ①–⑥
```

```go
type LVSGracefulDeploy struct {
    VIP          string
    Port         int
    OriginalWeight map[int]int    // nodeID -> 原权重
    DrainTimeout time.Duration    // 排空超时，默认 120s
    ObserveWindow time.Duration   // 观测窗口，默认 60s
}

func (d *LVSGracefulDeploy) DeployOne(ctx context.Context, nodeID int, order *ChangeOrder) error {
    // ① 摘除
    if err := d.setWeight(ctx, nodeID, 0); err != nil { return err }
    defer func() { /* 无论如何都要尝试加回 */ }()

    // ② 排空
    if err := d.waitDrain(ctx, nodeID); err != nil {
        d.setWeight(ctx, nodeID, d.OriginalWeight[nodeID])   // 恢复原权重
        return fmt.Errorf("排空超时: %w", err)
    }

    // ③ 变更（复用 T032 的原子落盘）
    if err := d.executor.Deploy(ctx, ...); err != nil {
        d.setWeight(ctx, nodeID, d.OriginalWeight[nodeID])
        return err
    }

    // ④ 探活
    if ok, _ := d.probe(ctx, nodeID); !ok {
        d.rollback(ctx, nodeID)                              // 回滚
        d.setWeight(ctx, nodeID, d.OriginalWeight[nodeID])
        return errors.New("探活失败，已回滚")
    }

    // ⑤ 加回
    if err := d.setWeight(ctx, nodeID, d.OriginalWeight[nodeID]); err != nil { return err }

    // ⑥ 观测
    return d.observe(ctx, nodeID, d.ObserveWindow)
}

// ★ 排空等待
func (d *LVSGracefulDeploy) waitDrain(ctx context.Context, nodeID int) error {
    deadline := time.Now().Add(d.DrainTimeout)
    for time.Now().Before(deadline) {
        conns, err := d.getActiveConns(ctx, nodeID)
        if err != nil { return err }
        if conns <= 1 { return nil }        // 留一点余量
        time.Sleep(2 * time.Second)
    }
    return errors.New("排空超时")
}
```

**为什么这是 2 节点规模的正确灰度方式**：

> 传统灰度用"5% → 20% → 100%"的流量百分比。但 Nginx 配置变更**无法按百分比切分** —— 配置是全量生效的。
>
> LVS 权重摘除的优势：把节点**整体摘出流量池**，变更完成并验证后再加回。**摘除期间用户侧零 5xx**（因为另一台 RS 在承接全部流量）。这是 LVS+DR 相对纯 Nginx reload 的核心优势。

**验收命令**：
```bash
go test ./internal/domain/lvs/... -run TestGraceful -v

# ★ 真机验证（最关键的验收）
# 1. 持续压测 VIP，观察错误率
while true; do curl -s -o /dev/null -w "%{http_code}\n" http://10.0.1.100/; sleep 0.1; done | sort | uniq -c

# 2. 在另一个终端触发配置变更
curl -s -X POST http://localhost:8080/api/v1/change-orders -d '{...}'

# 3. 期望：整个变更过程中，压测输出里 5xx 计数为 0
#    且能看到 RS1 的权重变化：1 → 0 → 1

# 4. 验证摘除生效
watch -n 1 "ipvsadm -Ln | grep -A2 '10.0.1.11'"
```

**陷阱** ★⚠️：
- **摘除（w=0）不会断开已有连接** —— 必须等排空。这就是为什么需要步骤 ②
- **排空超时的处理**：不能无限等。超时后要**恢复原权重并中止变更**，而不是强行变更
- **`defer` 里必须尝试加回权重** —— 任何异常路径都不能把节点永久留在池外（否则故障时会雪上加霜）
- **两台 RS 不能同时摘除** —— 否则全部流量中断。串行是硬约束
- **权重操作必须在 Director 上执行**，不是 RS。要通过 Director 的 Agent 下发指令
- 观测窗口要对比**基线指标**（变更前的错误率/延迟），不能只看绝对值

---

## T036 · 审批流（可选开关）✅ 已完成（2026-09-04）

**目标**：高风险变更需要人工确认。

**依赖**：T030

**涉及文件**：
```
ent/schema/approval.go                 # 审批实体（order_id Unique / status 枚举 / expires_at）
ent/approval/  (ent 生成：approval.go, _create/_update/_query/_delete.go)
internal/domain/deploy/approval.go     # 规则引擎 + 自审批拦截 + 超时过期 + 审计查询
internal/domain/deploy/order.go        # Submit 审批感知；Approve 自审批拦截；Reject 记录
internal/domain/deploy/state.go        # StatusDraft 增加 →pending 直达迁移
internal/server/handler/approval.go    # GET /api/v1/approvals、GET /api/v1/change-orders/:id/approval
internal/server/handler/deploy.go      # Submit 响应返回 approval_required + required_by
internal/server/router.go              # 挂载审批路由
internal/domain/deploy/approval_test.go# 9 例（规则/提交/审批/拒绝/过期/列表）
web/src/components/deploy/ApprovalPanel.vue   # ★ 前端面板推到 T039 与发布页一起落地
```

**实现说明（与原任务注释的偏差）**：
- 原注释用 CEL 表达式 `condition: "cluster == 'prod-web' && node_count >= 2"` 描述规则。本项目**不设表达式引擎依赖**，改为结构化字段 `ApprovalRule{Name, Enabled, Types, Sources, MinNodes}` 等价表达（Types 空=任意类型、MinNodes=0 不限制节点数），由 `ruleMatches` 做与运算匹配——后续如需「路径 / 集群标签」等更复杂条件可扩展结构体字段，无需引入 CEL。
- `Approval` 实体用字符串 `approver`（与变更单 `created_by` 同源，均为账号标识）而非原契约的 `approver_id *int`——本系统身份是账号字符串，无整数 user 表，避免引入无用的关联。
- `Submit` 落库逻辑：命中规则（或 `Strategy.ApprovalRequired` 显式声明）即 `createApproval` + 转入 `pending_approval`；免审批直接 `draft → pending`（故 `StatusDraft` 出边新增 `pending`，状态机仍闭合）。
- **自审批硬约束**：`AllowSelfApproval=false`（默认）且 `approver==created_by` 时 `Approve` 返回 `CodeInvalid`；配置开启后允许。
- **超时过期**：`createApproval` 写入 `expires_at=now+Timeout`；`ExpireApprovals` 由控制面定时 worker 调用，超时 pending → `expired` 且变更单自动 `rejected`。
- **证书自动续期豁免**：`Source=="auto_renew"` 一律免审批（不触发人工）。
- 端到端接线（Agent 执行、前端审批面板）随 M3 集成验收（T037 + T039）一并落地。

**契约**：

```go
type Approval struct {
    ID           int
    ChangeOrderID int
    RequiredBy   string     // 触发审批的规则名
    Status       string     // "pending" | "approved" | "rejected" | "expired"
    ApproverID   *int
    Comment      string
    CreatedAt, DecidedAt time.Time
    ExpiresAt    time.Time  // 超时自动拒绝
}

// 需要审批的规则（可配置）
type ApprovalRule struct {
    Name      string
    Condition string    // CEL 表达式或简单规则
    Enabled   bool
}
```

**默认规则**：
```yaml
approval:
  enabled: true
  rules:
    - name: "生产集群全量变更"
      condition: "cluster == 'prod-web' && node_count >= 2"
      approvers: ["admin"]
    - name: "nginx 主配置变更"
      condition: "path == 'nginx.conf'"
    - name: "LVS 配置变更"
      condition: "type == 'lvs'"
    - name: "二进制升级"
      condition: "type == 'upgrade'"
  timeout: 24h          # 超时自动拒绝
  allow_self_approval: false    # 提交者不能审批自己的变更
```

**验收命令**：
```bash
go test ./internal/domain/deploy/... -run TestApproval -v
# 覆盖：
#   - 命中规则 → 状态 pending，不自动执行
#   - 未命中 → 直接执行
#   - 审批通过 → 继续执行
#   - 审批拒绝 → 状态 rejected
#   - 超时 → expired
#   - 自己审自己 → 被拒绝

curl -s -X POST http://localhost:8080/api/v1/change-orders/16/approve -d '{"comment":"ok"}' | jq
```

**陷阱** ⚠️：
- ⚠️ `allow_self_approval` 默认 false —— 这是审计合规的基本要求
- ⚠️ 审批超时要有 worker 定期扫描并标记 expired
- ⚠️ **证书自动续期这类自动化变更不应触发审批** —— 规则里要排除 `source == 'auto_renew'`

---

## T037 · 发布进度实时推送（SSE）✅ 已完成（2026-09-04）

**目标**：让用户在页面上看到发布的每一步。

**依赖**：T030, T032

**涉及文件**：
```
internal/domain/deploy/events.go        # DeployEvent 数据模型 + EventSink 接口（域层只产生事件）
internal/server/hub.go                  # 进程内发布/订阅中心（实现 deploy.EventSink + 有界历史回放）
internal/server/handler/stream.go       # GET /api/v1/change-orders/:id/stream（SSE，依赖 DeployEventSource 接口避免循环依赖）
internal/server/server.go               # 创建 Hub + deploySvc.SetEventSink(hub)
internal/server/router.go              # 挂载 /:id/stream 路由
web/src/composables/useDeployStream.ts  # ★ 前端 composable 推到 T039 与发布页一起落地
web/src/components/deploy/ProgressPanel.vue
```

**实现说明（与原任务注释的偏差）**：
- `DeployEvent` 与 `EventSink` 定义在**域层** `internal/domain/deploy/events.go`：Service 经 `SetEventSink` 注入出口，发出提交/批准/拒绝/取消/开始 5 类生命周期事件（`emit` 在 `eventSink==nil` 时静默丢弃，单测与未接线态安全）。
- `Hub`（`internal/server/hub.go`）同时实现 `deploy.EventSink`（供域层 `Emit`）与 `handler.DeployEventSource` 接口（供 SSE 订阅）。**关键架构点**：handler 不 import server 包，而是依赖自己定义的 `DeployEventSource` 接口，规避 `server→handler` 的循环依赖。
- SSE 处理器设 `Content-Type: text/event-stream` + `X-Accel-Buffering: no`，逐事件 `Flush`，15s 心跳保活；每条事件带 `id: <全局单调号>`，支持客户端 `Last-Event-ID` 断连重连（Hub 在 `Subscribe` 时回放该变更单有界历史，默认每单保留最近 256 条）。
- 订阅者消费慢时 `Emit` 走 `default` 丢弃本次推送而非阻塞发布路径，靠重连补帧兜底——保证发布主路径不被 SSE 客户端拖慢。
- **刻意未做（推 T039）**：① 事件「同时落库」用于历史审计与跨进程重放——本期 Hub 为内存实现，`NewHub` 接口稳定，T039 接 PG 持久化时不改调用方；② 单节点执行步骤事件（transfer/validate/snapshot/...）待 T039 执行器经 Agent 接线后由域层在 9 步流水线中各发一步；③ 前端 `useDeployStream` + `ProgressPanel` 随发布页集成落地。
- **测试**：`internal/server/hub_test.go` 5 例（发布订阅 / 不串流 / 历史回放 / 取消无泄漏 / `-race` 并发安全）；`internal/domain/deploy/events_test.go` 2 例（提交/批准事件发出）。`go build/vet/gofmt` 干净。

**契约**：

```go
// 服务端：SSE 事件流
// GET /api/v1/change-orders/:id/stream
// Content-Type: text/event-stream

type DeployEvent struct {
    OrderID   int
    NodeID    int
    NodeName  string
    Step      string    // "transfer" | "validate" | ...
    Status    string    // "running" | "success" | "failed"
    Progress  int       // 0-100
    Message   string
    Timestamp time.Time
}

type Hub struct {
    subscribers map[int][]*Subscriber    // orderID -> 订阅者
    mu sync.RWMutex
}
func (h *Hub) Publish(evt DeployEvent)
func (h *Hub) Subscribe(orderID int) <-chan DeployEvent
```

**前端**：
```ts
export function useDeployStream(orderId: number) {
  const events = ref<DeployEvent[]>([])
  const progress = computed(() => ...)
  let es: EventSource | null = null

  function connect() {
    es = new EventSource(`/api/v1/change-orders/${orderId}/stream`)
    es.onmessage = (e) => events.value.push(JSON.parse(e.data))
    es.onerror = () => {
      // ★ 断连自动重连，且从 Last-Event-ID 恢复
      setTimeout(connect, 3000)
    }
  }
  onUnmounted(() => es?.close())
  return { events, progress, connect }
}
```

**验收命令**：
```bash
# 终端 1：订阅
curl -N http://localhost:8080/api/v1/change-orders/17/stream

# 终端 2：触发发布
curl -s -X POST http://localhost:8080/api/v1/change-orders/17/execute

# 期望：终端 1 实时输出每一步的事件流，形如：
# data: {"order_id":17,"node_id":1,"step":"validate","status":"running",...}
# data: {"order_id":17,"node_id":1,"step":"validate","status":"success",...}
# data: {"order_id":17,"node_id":1,"step":"snapshot","status":"running",...}

go test ./internal/server/... -run TestHub -race -v
```

**陷阱** ⚠️：
- ⚠️ **SSE 断连要自动重连** —— 用 `Last-Event-ID` header 恢复，不要从头重放
- ⚠️ Hub 的订阅者要在**断连时清理**（`defer unsubscribe`），否则内存泄漏
- ⚠️ gin 的 SSE 要禁用响应缓冲：`c.Writer.Flush()`
- ⚠️ 事件要**同时落库**（用于历史回放与审计），不能只在内存里

---

## T038 · 并发控制与任务队列 ✅ 已完成（2026-09-04）

**目标**：防止多个人同时改同一批节点。

**依赖**：T030

**涉及文件**：
```
ent/schema/deploy_node_lock.go          # 节点锁持久化行（node_id 唯一）
ent/deploynodelock*/ent/deploynodelock/*# ent 生成
internal/domain/deploy/lock.go          # LockManager：Acquire/Release/Refresh/HeldBy/CleanExpired
internal/domain/deploy/queue.go         # Queue：TryDequeue（PG SKIP LOCKED 的可移植等价）
internal/domain/deploy/worker.go        # Worker：轮询调度 + 锁生命周期 + 注入式 Runner
internal/domain/deploy/queue_test.go    # 锁互斥/过期复用/释放/队列串行/全局并行守卫 6 例
internal/domain/deploy/worker_test.go   # Worker 执行 + 锁释放 2 例
```

**实现说明（与原任务注释的偏差）**：
- 新增 `deploy_node_lock` ent 实体（node_id 唯一 + order_id + locked_at + expires_at），节点锁即数据库行。**单节点串行**由 `node_id` 唯一约束天然保证，跨进程互斥由「事务内先清过期再插入 + 唯一冲突」实现，SQLite（事务）与 PG 行为一致，**无需 SKIP LOCKED 即可保证正确性**。
- `LockManager.Acquire` 在 `client.Tx` 内：先 `DELETE ... WHERE node_id=? AND expires_at<now`（清本节点过期锁），再 `CREATE`（唯一冲突→已被占用→返回 false）。`CleanExpired` 由控制面启动/定时 worker 调用，兜底崩溃遗留锁。
- `Queue.TryDequeue` 是 **PG `FOR UPDATE SKIP LOCKED` 的可移植等价**：单控制面进程下逐个尝试 pending 单的节点锁，抢不到全部节点锁的单子跳过看下一个；`MaxConcurrentOrders` 作为「全局并行上限守卫」（按 running 计数）。PG 生产环境如需更优吞吐可改 `SELECT ... FOR UPDATE SKIP LOCKED`，接口与行为不变。
- `Worker` 轮询 `TryDequeue` → `Service.Start(pending→running)` → 注入的 `Runner.Run` → `ReleaseByOrder` 释放全部节点锁；`Runner` 接口（T039 经 Agent 接 9 步流水线实现）未实现前 `nil` 安全（不翻状态、不执行）。worker 自身是单 goroutine 串行调度，天然单并发；多 worker/更高并行靠 `MaxConcurrentOrders` 守卫 + 锁。
- **刻意未做（推 T039）**：① Worker 的实际 `Runner`（经 Agent 执行 9 步 + 状态流转到 success/failed/rolling_back）；② 控制面启动 worker + 接入 `QueuePollInterval` 配置；③ PG 专属 `SKIP LOCKED` 快路径（接口稳定，性能优化项）。

**验收命令**：
```bash
go test ./internal/domain/deploy/... -run 'TestLockManager|TestQueue|TestWorker' -race -v
# 覆盖：同节点互斥 / 过期锁清理后复用 / 释放 / 同节点两单只取一把 / 全局并行上限 / worker 执行并释放锁
```

**契约**：

```go
// 节点级锁：同一节点同时只能有一个变更
type NodeLock struct {
    NodeID    int
    OrderID   int
    LockedAt  time.Time
    ExpiresAt time.Time     // 防止死锁，默认 30 分钟
}

// 用 PG 的 advisory lock 或 SKIP LOCKED 实现分布式锁
func (q *Queue) AcquireNodeLock(ctx, nodeID, orderID int) (bool, error)

// 任务队列：用 PG 的 SKIP LOCKED（不引入 Redis）
func (q *Queue) Dequeue(ctx context.Context) (*ChangeOrder, error) {
    // SELECT * FROM change_orders WHERE status='pending'
    // ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1
}
```

**并发规则**：
```yaml
deploy:
  max_concurrent_orders: 3        # 全局最多 3 个变更单并行
  max_concurrent_per_node: 1      # ★ 单节点串行（硬约束）
  lock_timeout: 30m
  queue_poll_interval: 2s
```

**验收命令**：
```bash
go test ./internal/domain/deploy/... -run TestQueue -race -v
# 覆盖：
#   - 两个变更单争抢同一节点 → 只有一个能拿到锁
#   - 锁超时自动释放
#   - 控制面重启后锁状态正确恢复（不能出现永久锁死）

# 并发验证
for i in 1 2 3; do
  curl -s -X POST http://localhost:8080/api/v1/change-orders -d "{\"node_ids\":[1],...}" &
done; wait
curl -s http://localhost:8080/api/v1/change-orders?status=running | jq '.data.total'
# 期望：<= 1（同一节点串行）
```

**陷阱** ⚠️：
- ⚠️ **锁必须有超时** —— 否则控制面崩溃后节点永久锁死。数据库里存 `expires_at`，启动时有 worker 清理
- ⚠️ 用 PG 的 `SKIP LOCKED` 而不是 Redis —— 符合"不引入 Redis"的约束
- ⚠️ **SQLite 不支持 `SKIP LOCKED`** —— 开发态用 `BEGIN IMMEDIATE` 事务 + 状态更新模拟，行为要一致

---

## T039 · 发布页面与集成验收

> **状态：✅ 已完成（M3 收口，最小可用闭环达成）**
> 交付：前端 `web/src/views/Deploy.vue`（列表 + 新建弹窗 + 详情抽屉 + SSE 实时时间线 + 状态机操作按钮）、`web/src/api/deploy.ts`；后端 `web/embed.go`(webui)/`embed_stub.go`(非 webui)、`Service.Complete`/`StartRollback`、Worker 接入服务启动、`Rollback` 接口；闭环集成测试 `closedloop_test.go`；正式部署至 192.168.5.50 并端到端验收（见 `docs/DEPLOY-192.168.5.50.md`）。
> **与原始 spec 的差异（如实）**：① 新建发布用「单弹窗」而非 4 步向导（文件树勾选/策略可视化/diff 摘要需配置与模板模块接入，留待使用时迭代）；② 详情的 9 步任务时间线以「生命周期事件时间线」呈现（submit/start/complete/rollback/worker「等待执行器接入」），逐节点 9 步流水线随 Agent 部署（Runner 接线）后由域层在 7 步流水线中各发一步；③ 未单独拆分 `ProgressPanel`/`RollbackDialog` 组件，回滚以抽屉内按钮 + 状态机门控实现。以上均为「先跑通闭环、再迭代富化」的合理裁剪。

**目标**：发布任务的可视化与操作入口。

**依赖**：T037, T035, T036

**涉及文件**：
```
web/src/views/Deploy.vue
web/src/api/deploy.ts
web/src/components/deploy/CreateWizard.vue       # 4 步向导
web/src/components/deploy/ProgressPanel.vue
web/src/components/deploy/TaskTimeline.vue
web/src/components/deploy/RollbackDialog.vue
```

**页面要求**（参照 `prototype/index.html` 的「发布任务」与「新建发布向导」）：

**列表页**：
- 变更单卡片：标题、类型徽章、状态、目标节点、发起人、时间
- 筛选：状态 / 类型 / 时间范围
- 每个进行中的变更单显示**实时进度条**

**详情页**（核心）：
- 左侧：9 步任务时间线（每个节点一行，每步一个图标，失败步骤红色并展开错误详情）
- 右侧：变更内容 diff、快照信息、审批记录
- 顶部操作：暂停 / 继续 / **回滚**（红色按钮，需二次确认）

**新建发布向导（4 步）**：
```
① 选择配置  → 文件树勾选，或选模板
② 选择目标  → 节点/集群多选，显示影响面（几条配置、几个节点）
③ 发布策略  → 串行/批量/全量/★LVS优雅；观测窗口；失败阈值；自动回滚开关
④ 确认提交  → 显示 diff 摘要 + 风险评估 + 提交按钮
```

**验收命令**：
```bash
cd web && pnpm build && pnpm dev
# 浏览器验证：
#   - 新建发布向导 4 步能走通
#   - 提交后详情页能看到实时进度（9 步逐步点亮）
#   - 失败时步骤变红，显示错误详情
#   - 回滚按钮可用，二次确认后执行
```

---

## M3 集成验收 ★★ 项目最关键的验收

```bash
# ★ 场景 1：零污染（最重要）
# 下发一个语法错误的配置
cat > /tmp/bad.conf <<'EOF'
server {
    lstne 80;              # 故意拼错
    server_name example.com;
}
EOF
ORDER=$(curl -s -X POST http://localhost:8080/api/v1/change-orders \
  -d "{\"title\":\"测试-错误配置\",\"node_ids\":[1,2],\"path\":\"conf.d/bad.conf\",\"content\":\"$(cat /tmp/bad.conf)\"}" \
  | jq -r '.data.id')

curl -s -X POST "http://localhost:8080/api/v1/change-orders/$ORDER/execute"
sleep 20
curl -s "http://localhost:8080/api/v1/change-orders/$ORDER" | jq '.data.status'
# 期望：failed
ssh rs-nginx-01 "ls /etc/nginx/conf.d/bad.conf"     # 期望：No such file（零污染）
ssh rs-nginx-01 "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1/"
# 期望：200（nginx 仍在正常服务）

# ★ 场景 2：自动回滚（配置语法正确但导致 502）
# upstream 指向不存在的端口
# 期望：探活失败 → 自动从快照回滚 → 节点恢复 → 变更单 failed

# ★ 场景 3：LVS 优雅发布零 5xx
# 终端 1 持续压测 VIP
while true; do curl -s -o /dev/null -w "%{http_code}\n" http://10.0.1.100/; sleep 0.05; done > /tmp/probe.log &
# 终端 2 触发一个正常配置变更
# 结束后
grep -c "^5" /tmp/probe.log        # 期望：0
# 并验证权重变化
ipvsadm -Ln | grep 10.0.1.11       # 权重应回到原值

# ★ 场景 4：并发控制
# 同时对同一节点发起 3 个变更 → 只有一个能执行

# ★ 场景 5：审计完整
curl -s "http://localhost:8080/api/v1/audit?resource=change_order" | jq '.data.total'
# 期望：>= 场景数，且每条有操作人、时间、内容
```

**全部通过，平台即可投入实际使用。** 后续 M4–M9 是增值模块，可以边用边做。
