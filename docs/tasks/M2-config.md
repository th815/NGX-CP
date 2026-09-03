# M2 · 配置中心（W3–W4）

> **目标**：让配置可见、可比、可编辑、可校验。
> **完成标志**：改一个 conf 能看到 diff 与校验结果；写错的配置被拦下，绝不落盘。

---

## T021 · 配置文件树同步与内容寻址存储

**目标**：把 Agent 上报的配置树存入数据库，相同内容只存一份。

**依赖**：T017, T006

**涉及文件**：
```
ent/schema/config_file.go
ent/schema/config_blob.go
ent/schema/config_revision.go
internal/domain/config/store.go
internal/domain/config/store_test.go
internal/server/handler/config.go
```

**契约**：

```go
// 内容寻址：相同内容只存一份
type ConfigBlob struct {
    SHA256  string    // 主键，内容哈希
    Content []byte
    Size    int
    RefCount int      // 引用计数，为 0 时可 GC
}

// 逻辑文件（跨节点同名但内容可能不同）
type ConfigFile struct {
    NodeID  int
    Path    string          // /etc/nginx/conf.d/api.conf
    CurrentRevisionID int
}

// 版本
type ConfigRevision struct {
    FileID      int
    BlobSHA     string
    ParentID    *int        // 版本链
    Author      string      // 谁改的（用户名 or "system"）
    ChangeOrderID *int      // 由哪次变更产生
    Source      string      // "sync" | "manual_edit" | "cert_renew" | "security_block" | "rollback"
    CreatedAt   time.Time
    Comment     string
}

type ConfigStore interface {
    // 同步 Agent 上报的配置树，返回是否有变化
    SyncFromAgent(ctx context.Context, nodeID int, files []agentv1.ConfigFile) (changed int, err error)

    // 内容寻址写入
    PutBlob(ctx context.Context, content []byte) (sha string, err error)
    GetBlob(ctx context.Context, sha string) ([]byte, error)

    // 创建新版本
    CreateRevision(ctx context.Context, fileID int, content []byte, opts RevisionOpts) (*ConfigRevision, error)

    // 列版本
    ListRevisions(ctx context.Context, fileID int, limit int) ([]*ConfigRevision, error)
}
```

**验收命令**：
```bash
go generate ./ent && make migrate-dev
go test ./internal/domain/config/... -run TestSyncFromAgent -v
# 覆盖：
#   - 首次同步 → 创建 file + blob + revision(source=sync)
#   - 内容未变再同步 → 不产生新版本，changed=0
#   - 内容变化 → 产生新版本，parent 指向上一版
#   - 两个节点有相同内容的文件 → blob 只有一份（去重生效）

# SQL 验证去重
psql -c "SELECT count(*) FROM config_blobs;"     # 应等于去重后的内容数
```

**陷阱** ⚠️：
- ⚠️ **`ParentID` 版本链要在事务里创建**，否则并发写入会断链
- ⚠️ Blob 的 `RefCount` 更新要小心；简化做法：**不删 blob**（配置内容很小，几十 MB 无所谓），避免引用计数 bug
- ⚠️ 大文件（> 1MB）考虑压缩存储，但配置通常 < 100KB

---

## T022 · 版本链与 Diff 计算

**目标**：任意两版配置的差异可视化。

**依赖**：T021

**涉及文件**：
```
internal/domain/config/diff.go
internal/domain/config/diff_test.go
internal/pkg/nginxconf/formatter.go
```

**契约**：

```go
type DiffResult struct {
    OldRev   int
    NewRev   int
    Hunks    []Hunk
    Stats    DiffStats
}

type Hunk struct {
    OldStart, OldLines int
    NewStart, NewLines int
    Lines []DiffLine
}

type DiffLine struct {
    Type    string   // "add" | "del" | "context"
    Content string
    OldNo   int      // 0 表示新增行
    NewNo   int
}

type DiffStats struct {
    Added, Deleted, Changed int
}

// 用 gotextdiff（Myers 算法，与 git 相同）
func Diff(oldContent, newContent string) *DiffResult

// 按 nginx 语义智能 diff：对齐 block 而非纯文本行
// （可选增强：先格式化再 diff，避免缩进变化造成大量噪音）
func DiffNginx(oldContent, newContent string) *DiffResult
```

**验收命令**：
```bash
go test ./internal/domain/config/... -run TestDiff -v
# 用例：
#   - 新增一行 → Added=1
#   - 删除一行 → Deleted=1
#   - 修改一行 → 1 del + 1 add
#   - 大文件（5000 行）diff 耗时 < 100ms
#   - 缩进变化（格式化后）→ 不产生 diff 噪音

go test -bench=BenchmarkDiff ./internal/domain/config/
# 期望：100KB 配置 diff < 50ms
```

**陷阱** ⚠️：
- ⚠️ **纯文本 diff 对配置不友好** —— 缩进调整会产生大量噪音。建议先做 nginx 格式化（统一缩进、对齐 `;`）再 diff
- ⚠️ 前端渲染大 diff 会卡，**超过 2000 行的 diff 要折叠 context 或分页**
- ⚠️ 用 `github.com/hexops/gotextdiff`（Go 官方 gopls 用的库），不要自己实现 Myers

---

## T023 · 配置编辑器（Monaco + nginx 语法）

**目标**：在浏览器里编辑 nginx 配置，带语法高亮。

**依赖**：T010, T021

**涉及文件**：
```
web/src/views/Configs.vue
web/src/components/editor/NginxEditor.vue
web/src/components/editor/MonacoSetup.ts        # 注册 nginx 语言
web/src/components/editor/nginx-language.ts     # 语法定义
web/src/components/editor/DiffViewer.vue
```

**契约**：

```ts
// nginx 语言定义（Monaco 的 Monarch tokenizer）
export const nginxLanguage = {
  defaultToken: '',
  keywords: ['server','location','upstream','http','stream','events','mail','if','include', ...],
  directives: ['listen','server_name','proxy_pass','root','index','access_log','error_log', ...],
  tokenizer: {
    root: [
      [/^\s*#.*$/, 'comment'],
      [/\$[\w]+/, 'variable'],            // $remote_addr
      [/\b\d+[kKmMgG]?\b/, 'number'],
      [/[a-zA-Z_][\w_]*/, { cases: {
          '@keywords': 'keyword',
          '@directives': 'type',
          '@default': 'identifier' }}],
      [/{/, 'delimiter.curly'], [/}/, 'delimiter.curly'],
      [/;/, 'delimiter'],
    ]
  }
}
```

**编辑器功能要求**：
- 语法高亮（指令 / 变量 / 注释 / 数字 / 字符串）
- 行号、括号匹配、代码折叠（`{}` 块）
- Ctrl+S 保存 → 触发校验 → 创建草稿
- **实时校验**（debounce 800ms）：调 API 做 `nginx -t`，错误在编辑器里画波浪线
- Diff 视图：并排显示当前版本 vs 草稿

**验收命令**：
```bash
cd web && pnpm build       # 无 TS 错误，且 Monaco 在独立 chunk（检查 dist 产物）
pnpm dev
# 浏览器验证：
#   - 打开配置中心，选一个文件，能正常高亮
#   - 输入 "lstne 80;" → 出现红色波浪线，提示 unknown directive
#   - Ctrl+S → 弹出保存确认，显示 diff
```

**陷阱** ⚠️：
- ⚠️ **Monaco 体积 3MB+，必须懒加载**（`defineAsyncComponent` 或动态 import），不要进主 bundle
- ⚠️ Monaco 的 worker 需要配 `vite.config.ts` 的 `optimizeDeps.exclude` 或用 `monaco-editor-webpack-plugin` 的 Vite 等价方案
- ⚠️ nginx 配置里 `$` 开头是变量，正则要转义

---

## T024 · 配置校验（一）：nginx -t（Agent 侧）

**目标**：在目标节点上用完整上下文做语法校验。

**依赖**：T017, T011

**涉及文件**：
```
internal/agent/executor/validate.go
internal/agent/executor/validate_test.go
internal/domain/config/validate.go
internal/server/handler/validate.go
proto/agent/v1/agent.proto          # 追加 ValidateConfig RPC
```

**契约**：

```go
// Agent 侧：在 staging 目录里校验
func (e *Executor) Validate(ctx context.Context, req *ValidateRequest) (*ValidateResponse, error) {
    // 1. 把待校验的所有文件写到 staging 目录（保持目录结构）
    // 2. ★ 必须用 -p 指定 prefix，保证相对路径（include conf.d/*.conf）语义一致
    cmd := exec.CommandContext(ctx, e.nginxPath, "-t",
        "-p", e.prefix,                    // ★ 关键
        "-c", e.stagingConfPath)           // staging 里的 nginx.conf
    // 3. 解析输出
}

type ValidateResponse struct {
    OK      bool
    Errors  []NginxError     // 结构化解析
    Raw     string           // 原始输出
}

type NginxError struct {
    Level   string   // "emerg" | "alert" | "crit" | "error" | "warn"
    Message string
    File    string
    Line    int
}
```

**错误解析**：
```
nginx: [emerg] unknown directive "lstne" in /etc/nginx/conf.d/api.conf:12
  → {Level:"emerg", Message:`unknown directive "lstne"`, File:"/etc/nginx/conf.d/api.conf", Line:12}

nginx: [emerg] host not found in upstream "nonexistent" in /etc/nginx/conf.d/api.conf:5
nginx: the configuration file /etc/nginx/nginx.conf syntax is ok
nginx: configuration file /etc/nginx/nginx.conf test is successful
  → OK=true
```

**验收命令**：
```bash
go test ./internal/agent/executor/... -run TestValidate -v
# 用例（用 testdata 里的真实输出）：
#   - 正确配置 → OK=true
#   - unknown directive → OK=false, Line 正确
#   - host not found in upstream → OK=false
#   - 端口被占用（bind() failed）→ OK=false（这种是运行时问题，要区分）

# 真机验证
curl -s -X POST http://localhost:8080/api/v1/configs/validate -d @testdata/bad.conf | jq
# 期望：{"code":4012,"message":"配置语法错误","detail":"nginx: [emerg] ..."}
```

**陷阱** ★⚠️：
- **必须用 `nginx -t -p <prefix> -c <conf>`，不能单独校验一个 conf 文件** —— 单独校验时 `include conf.d/*.conf` 的相对路径解析会错，导致误判
- **`-t` 成功不代表配置语义正确**（如 upstream 指向不存在但 DNS 能解析的主机）。所以还需要 T025 的语义校验
- **`bind() to 0.0.0.0:80 failed (98: Address already in use)` 是 warning 不是 error** —— 因为已有 nginx 占用端口是正常现象。要区分 emerg/warn
- staging 目录必须与 `/etc/nginx` 同分区（T018 已检查），否则降级

---

## T025 · 配置校验（二）：语义规则与能力基线

**目标**：在 `nginx -t` 之外，做平台特有的语义检查。

**依赖**：T016, T024

**涉及文件**：
```
internal/domain/config/rules/registry.go
internal/domain/config/rules/module_check.go       # 用了模块但节点没编译
internal/domain/config/rules/cert_check.go         # ssl_certificate 引用的证书是否存在
internal/domain/config/rules/upstream_check.go     # upstream server 可达性
internal/domain/config/rules/stream_check.go       # stream 块相关
internal/domain/config/rules/dr_check.go           # DR 模式相关
internal/domain/config/rules/rules_test.go
configs/rules.yaml
```

**契约**：

```go
type Rule interface {
    ID() string
    Name() string
    Severity() string        // "error" | "warning" | "info"
    Check(ctx context.Context, in *CheckInput) []Issue
}

type CheckInput struct {
    ConfigFiles []ConfigFile          // 待校验的完整配置树
    Capability  *Capability           // 目标节点的能力
    Node        *Node
}

type Issue struct {
    RuleID   string
    Severity string
    Message  string
    File     string
    Line     int
    Fix      string       // 修复建议
}

// 规则注册表
var registry = []Rule{
    &ModuleCheckRule{},      // ★ 最重要
    &CertRefRule{},
    &UpstreamReachRule{},
    &PortConflictRule{},
    &StreamBlockRule{},
    &DRPortRule{},
    &SecurityRule{},         // 如：server_tokens 应关闭、不应暴露 .git
}
```

**核心规则：ModuleCheckRule**（双机一致性的守护者）

```go
// 场景：配置里用了 check 指令（nginx_upstream_check_module）
//       但目标节点没有编译这个模块 → nginx -t 会报 unknown directive
//       更危险的是：两台 RS 编译参数不同，一台通过一台失败
func (r *ModuleCheckRule) Check(ctx, in) []Issue {
    required := extractRequiredModules(in.ConfigFiles)   // 从指令推断需要的模块
    for _, m := range required {
        if !in.Capability.HasModule(m) {
            return Issue{Severity:"error",
                Message: fmt.Sprintf("配置使用了需要模块 %s 的指令，但节点 %s 未编译该模块", m, node.Name),
                Fix: "重新编译 nginx 并加入该模块，或从配置中移除相关指令"}
        }
    }
    // ★ 还要对比集群内其他节点：如果同类节点有这个模块而它没有 → 告警配置漂移
}
```

**指令 → 模块映射**：
```yaml
# configs/rules.yaml
module_requirements:
  check:              nginx_upstream_check_module
  ssl_certificate:    http_ssl
  http2:              http_v2
  http3:              http_v3
  quic:               http_v3
  brotli:             ngx_brotli
  real_ip_header:     http_realip
  stub_status:        http_stub_status
  # stream 块
  preread:            stream_ssl_preread
```

**验收命令**：
```bash
go test ./internal/domain/config/rules/... -v
# 每个规则至少 2 个用例（通过 / 不通过）

# 关键用例：
#   - 配置含 "check interval=3000" 且节点有 check 模块 → 无 issue
#   - 同上但节点无 check 模块 → error + 修复建议
#   - 配置引用 ssl_certificate 但证书不存在 → error
#   - stream{} 块存在但节点未编译 --with-stream → error
#   - server_tokens on → warning（信息泄露）

# 真机验证：在 RS1 上编辑配置加 check 指令，RS2 不加 → 校验应提示两台结果不同
```

**陷阱** ⚠️：
- ⚠️ **`nginx -t` 已经能捕获大部分模块缺失**（unknown directive）。这条规则的价值在于**提前发现双机不一致** —— 在一台能过一台不能过之前就告警
- ⚠️ 规则要可配置开关（`configs/rules.yaml`），不要硬编码
- ⚠️ 每条规则都要给 `Fix` 建议，否则用户拿到报错不知道怎么办

---

## T026 · 配置漂移检测

**目标**：发现节点上的实际配置与平台期望版本不一致。

**依赖**：T021

**涉及文件**：
```
internal/domain/config/drift.go
internal/domain/config/drift_test.go
internal/domain/config/drift_worker.go     # 定时巡检
```

**契约**：

```go
type DriftReport struct {
    NodeID    int
    CheckedAt time.Time
    Items     []DriftItem
}

type DriftItem struct {
    Path          string
    ExpectedSHA   string      // 平台记录的当前版本
    ActualSHA     string      // 节点上实际的
    Diff          *DiffResult
    DetectedAt    time.Time
    Severity      string      // "critical" | "warning"
}

type DriftDetector interface {
    // 对比节点上报的配置树与数据库里的期望版本
    Detect(ctx context.Context, nodeID int, actual []ConfigFile) (*DriftReport, error)

    // 定时巡检（默认 5 分钟）
    RunWorker(ctx context.Context, interval time.Duration) error
}
```

**漂移的三种类型**：
1. **手工改动** —— 有人在机器上直接 vi 改了配置（最常见，也最危险）
2. **同步失败** —— 发布后部分节点成功部分失败
3. **文件被外部程序修改** —— 如 certbot 自动改了 ssl 配置

**处理策略**（可配置）：
```yaml
config:
  drift:
    check_interval: 5m
    auto_alert: true
    auto_remediate: false        # 默认不自动修复！只告警
    severity_rules:
      - path_pattern: "conf.d/*.conf"
        severity: critical
      - path_pattern: "nginx.conf"
        severity: critical
```

**验收命令**：
```bash
go test ./internal/domain/config/... -run TestDrift -v

# 真机验证：手工在 RS 上改配置
ssh rs-nginx-01 "echo '# test drift' >> /etc/nginx/conf.d/api.conf"
sleep 300       # 等巡检
curl -s http://localhost:8080/api/v1/configs/drift | jq '.data.items[] | {node, path, severity}'
# 期望：检测到 rs-nginx-01 的 api.conf 漂移，severity=critical

# 告警应出现在 Dashboard 与告警中心
```

**陷阱** ⚠️：
- ⚠️ **默认绝不自动修复漂移** —— 手工改动可能是紧急修复，自动覆盖会毁掉它。只告警，让人决定
- ⚠️ 漂移检测要在**心跳或定期上报**时做，不要每次都跑 `nginx -T`（开销大）。用文件 mtime + size 快速判断，变化了再拉内容
- ⚠️ 时间精度：文件 mtime 是秒级，快速连续修改可能漏检。以 SHA256 为准

---

## T027 · 配置模板与三级变量

**目标**：让"一处改全局达"成为可能。

**依赖**：T021, T022

**涉及文件**：
```
ent/schema/config_template.go
ent/schema/config_variable.go
internal/domain/config/template.go
internal/domain/config/template_test.go
internal/domain/config/render.go
configs/templates/upstream.conf.tmpl
```

**契约**：

```go
// 三级变量，优先级从低到高
type VariableScope string
const (
    ScopeGlobal   VariableScope = "global"    // 全平台
    ScopeCluster  VariableScope = "cluster"   // 集群（如 prod-web）
    ScopeNode     VariableScope = "node"      // 单节点
)

type Variable struct {
    Scope    VariableScope
    TargetID int        // cluster_id 或 node_id，global 时为 0
    Key      string
    Value    string
    Secret   bool       // 敏感值，API 返回时打码
}

type ConfigTemplate struct {
    Name       string
    Content    string      // Go template 语法
    AppliesTo  string      // 目标路径模式，如 "conf.d/upstream-{cluster}.conf"
    Variables  []string    // 模板里引用的变量清单（自动提取）
}

// 渲染：变量按 global < cluster < node 覆盖
func Render(tmpl *ConfigTemplate, vars map[string]string) (string, error)

// 批量渲染到多个节点
func RenderForNodes(ctx, tmpl, nodeIDs []int) (map[int]string, error)
```

**模板示例**：
```gotemplate
{{/* configs/templates/upstream.conf.tmpl */}}
upstream {{ .cluster }}_backend {
    {{ range .backends }}
    server {{ .ip }}:{{ .port }} weight={{ .weight }} max_fails=3 fail_timeout=10s;
    {{ end }}
    check interval=3000 rise=2 fall=3 timeout=1000 type=tcp;
}
```

**验收命令**：
```bash
go test ./internal/domain/config/... -run TestRender -v
# 覆盖：
#   - 变量三级覆盖（node 覆盖 cluster 覆盖 global）
#   - 缺失变量 → 报错且明确指出缺哪个
#   - 模板语法错误 → 报错
#   - Secret 变量在 API 响应里打码为 ******

# 真机验证
curl -s -X POST http://localhost:8080/api/v1/templates/1/render -d '{"node_ids":[1,2]}' | jq
# 期望：返回两台节点各自的渲染结果（因变量不同内容不同）
```

**陷阱** ⚠️：
- ⚠️ **Secret 变量（如密码）不能进配置版本历史** —— 渲染后写入时要特别标记，diff 时打码
- ⚠️ **缺失变量必须报错而不是渲染成空字符串** —— 空字符串可能让 nginx 配置静默错误
- ⚠️ 模板渲染后的产物要**走完整的校验与发布流程**，不能绕过

---

## T028 · 配置中心页面

**目标**：配置树、编辑、diff、版本历史的完整 UI。

**依赖**：T023, T022, T026

**涉及文件**：
```
web/src/views/Configs.vue
web/src/api/configs.ts
web/src/components/config/FileTree.vue
web/src/components/config/RevisionList.vue
web/src/components/config/DriftPanel.vue
web/src/components/config/TemplateEditor.vue
```

**页面布局**（三栏）：
```
┌── 左：文件树 ──┬── 中：编辑器 ──┬── 右：信息面板 ──┐
│ 按节点分组      │  Monaco        │  Tab:           │
│ ☑ nginx.conf  │  语法高亮       │  · 版本历史      │
│ ☑ conf.d/     │  保存 → 校验    │  · 校验结果      │
│   ├ api.conf  │  Diff 切换      │  · 变量          │
│   └ ssl.conf  │                │  · 漂移状态      │
└──────────────┴────────────────┴─────────────────┘
```

**关键交互**：
- 文件树顶部：**节点选择器**（切换查看不同节点的配置）
- 多节点同名文件：显示"N 个节点有此文件"，可选择"对比节点"（双机一致性检查）
- 编辑 → Ctrl+S → **先校验后保存**：校验失败时拒绝保存并高亮错误行
- 版本历史：时间线，可点任意两版对比
- 漂移面板：红色徽章 + "查看差异" + "用平台版本覆盖"/"采纳节点版本"两个动作

**验收命令**：
```bash
cd web && pnpm build && pnpm dev
# 浏览器验证：
#   - 文件树正确显示 nginx.conf + conf.d/*
#   - 编辑加入语法错误 → 波浪线 + 保存被拒绝
#   - 版本历史能看到 T021 同步产生的版本
#   - 手工在节点改配置 → 漂移面板显示红色徽章
```

---

## T029 · 配置文件监听与实时同步

**目标**：节点上配置被改动时，平台尽快感知。

**依赖**：T026

**涉及文件**：
```
internal/agent/watcher/inotify.go
internal/agent/watcher/watcher.go
internal/agent/watcher/watcher_test.go
```

**契约**：

```go
// 用 fsnotify 监听 /etc/nginx（递归）
type Watcher struct {
    paths   []string
    handler func(event ConfigChangeEvent)
}

type ConfigChangeEvent struct {
    Path   string
    Op     string    // "write" | "create" | "remove" | "rename" | "chmod"
    Time   time.Time
    Actor  string    // 尽力而为：从 auditd / 进程信息推断是谁改的
}

func NewWatcher(paths []string) (*Watcher, error)
func (w *Watcher) Start(ctx context.Context) error
```

**行为**：
- 检测到变化 → **防抖 3 秒**（编辑器保存可能产生多个事件）→ 触发能力上报 → 控制面做漂移检测
- 不自动修复，只上报

**验收命令**：
```bash
go test ./internal/agent/watcher/... -v
# 用例：创建/修改/删除文件 → 产生对应事件；防抖生效（连续 5 次写只触发 1 次）

# 真机验证
ssh rs-nginx-01 "echo '# x' >> /etc/nginx/conf.d/api.conf"
sleep 10
curl -s http://localhost:8080/api/v1/configs/drift | jq '.data.items | length'
# 期望：10 秒内检测到漂移（比定时巡检的 5 分钟快很多）
```

**陷阱** ⚠️：
- ⚠️ **inotify watch 数量有限制**（`/proc/sys/fs/inotify/max_user_watches`，默认 8192）。目录多时要处理 `ENOSPC`，降级为定时轮询
- ⚠️ **日志轮转、编辑器临时文件（`.swp`、`~`）会产生大量噪音事件** —— 必须过滤
- ⚠️ Agent 退出时要正确关闭 watcher，否则 fd 泄漏

---

## M2 集成验收

```bash
make test && make lint

# 1. 配置同步
curl -s -X POST http://localhost:8080/api/v1/nodes/1/sync | jq
curl -s http://localhost:8080/api/v1/configs?node_id=1 | jq '.data.items[] | .path'
# 期望：列出 nginx.conf 与 conf.d/ 下所有文件

# 2. Diff
curl -s "http://localhost:8080/api/v1/configs/1/diff?from=1&to=2" | jq '.data.stats'
# 期望：{"added":N,"deleted":M,"changed":K}

# 3. ★ 校验拦截（最关键）
cat > /tmp/bad.conf <<'EOF'
server { lstne 80; }
EOF
curl -s -X POST http://localhost:8080/api/v1/configs/validate \
     -d "{\"node_id\":1,\"path\":\"conf.d/bad.conf\",\"content\":\"$(cat /tmp/bad.conf)\"}" | jq
# 期望：code=4012，detail 含 "unknown directive"
# 验证：节点上 /etc/nginx/conf.d/bad.conf 不存在（没有被写入）

# 4. 漂移检测
ssh rs-nginx-01 "echo '# drift' >> /etc/nginx/conf.d/api.conf"
sleep 15
curl -s http://localhost:8080/api/v1/configs/drift | jq '.data.items | length'
# 期望：>= 1

# 5. 双机一致性
curl -s http://localhost:8080/api/v1/configs/compare -d '{"node_ids":[1,2],"path":"conf.d/api.conf"}' | jq '.data.identical'
# 期望：false（因为手工改了一台）
```

**全部通过后才进入 M3（发布引擎）。**
