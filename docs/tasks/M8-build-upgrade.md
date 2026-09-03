# M8 · 构建与升级（W12）

> **目标**：管理 nginx 编译版本与模块（尤其 `nginx_upstream_check_module`）的升级，做到热升级全程 zero 5xx。
> **完成标志**：下发一个补丁版 nginx 升级 → 全程零 5xx → 检查 `nginx -V` 已更新。
>
> 决策依据：`docs/DECISIONS.md` §12（编译升级与模块管理）、§13（能力发现）。
> 关键约束：**模块版本必须随 nginx 版本走**；构建环境要可复现（匹配目标 glibc/gcc）；升级前必须做编译一致性 + DR 合规 + 快照。

---

## T080 · nginx 版本/模块矩阵模型

**目标**：记录每个 nginx 版本对应的模块版本与编译参数，供升级选型。

**依赖**：T006, T013(capability)

**涉及文件**：
```
ent/schema/nginx_build.go
internal/upgrade/matrix.go
internal/upgrade/matrix_test.go
```

**契约**：
```go
type NginxBuild struct {
    ID           int
    Version      string          // "1.30.0"
    OpenSSL      string          // "3.5.1"
    Modules      []ModuleVer     // [{name:"nginx_upstream_check_module", ver:"0.4.0"}]
    ConfigureArgs string         // 完整 --prefix=... --add-module=...
    ChecksumSHA  string
}
// 双机编译一致性：两台 nginx 的 ConfigureArgs 除路径外必须一致
```

**验收命令**：
```bash
go test ./internal/upgrade/... -run Matrix
# 期望：用 testdata/nginx_V_1.30.0.txt 解析出模块清单与编译参数
```

**AI 陷阱**：
- `nginx_upstream_check_module` 版本必须和 nginx 版本匹配，升级 nginx 时模块要同步升
- 解析 `nginx -V` 时注意 `--add-module=../xxx` 是相对路径，入库时转绝对或归一化

---

## T081 · 可复现构建环境

**目标**：容器化构建容器，匹配目标系统 glibc/gcc，产出可复现二进制。

**依赖**：T080

**涉及文件**：
```
build/Dockerfile.build
build/build.sh
internal/upgrade/builder.go
```

**契约**：
```bash
# 构建容器基于目标系统同版本基础镜像（如 rockylinux:9），保证 glibc 兼容
docker build -f build/Dockerfile.build -t ngx-builder .
./build/build.sh 1.30.1 upstream_check_module=0.4.1
# 产出：/dist/nginx-1.30.1-linux-amd64.tar.gz
```

**验收命令**：
```bash
docker run --rm ngx-builder nginx -V 2>&1 | grep -o "nginx/1.30.1"
# 期望：版本正确；产物在 /dist 且 sha256 稳定（相同输入相同输出）
```

**AI 陷阱**：
- 构建容器必须**与目标机同代 glibc**，否则二进制在目标机跑不了
- 编译参数从 T080 的 ConfigureArgs 来，别手写漏参（尤其 `--with-stream`、`--with-http_v3_module`）
- 可复现：固定依赖版本，否则两次构建产物 sha 不同

---

## T082 · 构建产物管理

**目标**：产物入库（artifact store）+ sha256 + 签名，可追溯。

**依赖**：T081

**涉及文件**：
```
internal/upgrade/artifact.go
internal/upgrade/artifact_test.go
```

**契约**：
```go
type Artifact struct {
    BuildID    int
    Path       string   // 对象存储或本地 /var/lib/ngxcp/artifacts/
    SHA256     string
    Signature  string   // 构建者签名，下发前验签
    CreatedAt  time.Time
}
```

**验收命令**：
```bash
go test ./internal/upgrade/... -run Artifact
# 期望：入库后 sha256 校验通过；篡改后验签失败
```

**AI 陷阱**：
- 下发前必须验签，防止构建链被投毒
- 产物归档保留，回滚时还能拿到旧版本

---

## T083 · 热升级指令（Agent）

**目标**：Agent `UpgradeBinary` —— 下载到 staging、测试、优雅升级（USR2 + QUIT）、探活、失败回滚。

**依赖**：T082, M1(gRPC)

**涉及文件**：
```
proto/agent/v1/agent.proto       # UpgradeBinary 已在全局契约
internal/agent/upgrade.go
```

**契约 / 执行序列**：
```
下载产物(mTLS) → sha256 校验 → staging 目录
  → 新二进制 nginx -t（用目标 conf）
  → 发 USR2 启动新 master（旧 worker 继续服务）
  → 探活（80+443 握手成功）
  → 发 QUIT 给旧 master（优雅退出旧 worker）
  → 失败则 QUIT 新 master + 重启旧 master
```

**验收命令**：
```bash
# 升级期间另开终端持续探活
while true; do curl -s -o /dev/null -w "%{http_code}\n" https://localhost; sleep 0.2; done
# 期望：全程 200，无 502/504 间隙
```

**AI 陷阱**：
- 必须用 USR2 平滑升级，**不能 kill -9 重启**
- 新 master 探活失败要能回退到旧 master，旧二进制别急着删
- `nginx -t` 要用目标 conf 的完整上下文

---

## T084 · 升级任务编排（zero 5xx）

**目标**：结合 LVS 权重摘除，串行升级 2 台，全程零 5xx。

**依赖**：T083, T054(LVS 灰度)

**涉及文件**：
```
internal/upgrade/orchestrator.go
internal/upgrade/orchestrator_test.go
```

**契约**：
```go
// 1+1 串行：先摘 RS2 权重 → 升级 RS2 → 探活 → 加回
//            再摘 RS1 → 升级 RS1 → 探活 → 加回
// 摘除期间流量只在另一台，用户侧零 5xx
func Orchestrate(upgrade UpgradeRequest) *ChangeOrder
```

**验收命令**：
```bash
make e2e TEST=upgrade_zero_5xx
# 期望：压测流量全程无 5xx；两台 nginx -V 均更新
```

**AI 陷阱**：
- 升级期间靠 LVS 权重摘除，不能指望 nginx reload（reload 不摘 upstream 成员）
- 串行 1+1，别并行两台同时升（会全断）

---

## T085 · 模块-nginx 兼容矩阵

**目标**：维护 `upstream_check_module ↔ nginx` 版本兼容表，升级时自动选兼容组合。

**依赖**：T080

**涉及文件**：
```
internal/upgrade/compat.go
internal/upgrade/compat_test.go
```

**契约**：
```go
// 给定目标 nginx 版本，返回兼容的模块版本集合
func CompatibleModules(nginxVer string) map[string]string
// 如 1.30.x → upstream_check_module 0.4.x
```

**验收命令**：
```bash
go test ./internal/upgrade/... -run Compat
# 期望：1.30.0 返回正确的模块版本；未知版本报错而非瞎猜
```

**AI 陷阱**：
- 未知版本**报错停下**，别用"最近似"猜测（可能编译失败）
- 模块升级要纳入同一变更单，不能只升 nginx 不升模块

---

## T086 · 升级前置检查

**目标**：升级前自动跑编译一致性 + DR 合规 + 磁盘 + 快照。

**依赖**：T013, T052, T082

**涉及文件**：
```
internal/upgrade/preflight.go
```

**契约**：
```go
type Preflight struct {
    CompileConsistent bool   // 双机编译参数一致
    DRCompliant       bool   // T052 全过
    DiskSpaceOK       bool
    SnapshotTaken     bool
}
func RunPreflight(nodeID int) Preflight
// 任一不过 → 阻断升级
```

**验收命令**：
```bash
go test ./internal/upgrade/... -run Preflight
# 期望：双机编译不一致时升级被阻断
```

**AI 陷阱**：
- 升级前**必须**做快照（T034），否则回滚无据
- DR 合规不过的节点不能升（升完可能破坏 LVS）

---

## T087 · 构建与升级 UI

**目标**：版本矩阵、构建触发、升级进度、LVS 联动可视化。

**依赖**：T080–T086

**涉及文件**：
```
web/src/views/build/{Matrix,Build,Upgrade}.vue
web/src/components/upgrade/ProgressTimeline.vue
```

**要点**：
- 版本矩阵表：nginx 版本 / 模块版本 / 编译参数 / 适用节点
- 构建页：选版本 + 模块 → 触发 T081 构建 → 进度条
- 升级页：选目标版本 → 显示前置检查结果 → 串行进度时间线（含 LVS 摘除/加回节点）

**验收命令**：
```bash
cd web && npm run build && npm run typecheck
# 期望：构建通过；升级页能展示串行进度
```

**AI 陷阱**：
- 升级进度时间线要展示 LVS 权重变化节点（摘除/加回），让操作可追溯
- 前置检查不过时禁用「开始升级」按钮

---

## T088 · 升级演练验收

**目标**：端到端验证 zero 5xx 升级。

**依赖**：T084, T087

**涉及文件**：
```
scripts/verify_upgrade.sh
```

**契约 / 验收**：
```bash
# 起压测，触发一次补丁版升级，全程采样状态码
./scripts/verify_upgrade.sh
# 期望：5xx 计数 = 0；两台 nginx -V 显示新版本
```

**AI 陷阱**：
- 演练要在**真实 LVS-DR 拓扑**上跑，单机验证不出问题但上线就 5xx
- 压测要覆盖长连接（keepalive），短连接掩盖切换间隙
