# M1 · 控制面骨架与 Agent 接入（W1–W2）

> **目标**：让 4 个节点（2 Director + 2 RealServer）接入平台，能力可见、状态可感。
> **完成标志**：4 个节点全部在线，Agent 掉线 30 秒内感知，节点详情页能看到完整的 nginx 能力基线、配置树、日志路径。

---

## T010 · 前端框架搭建

**目标**：Vue 3 + TypeScript + Naive UI 项目骨架，含布局与路由。

**依赖**：T001

**涉及文件**：
```
web/package.json
web/vite.config.ts
web/tsconfig.json
web/src/main.ts
web/src/App.vue
web/src/router/index.ts
web/src/layouts/MainLayout.vue        # 侧边栏 + 顶栏 + 内容区
web/src/stores/app.ts                 # Pinia
web/src/api/client.ts                 # axios 封装（统一错误处理）
```

**契约**：

```ts
// web/src/api/client.ts
import axios from 'axios'
import { useMessage } from 'naive-ui'

const client = axios.create({ baseURL: '/api/v1', timeout: 30000 })

client.interceptors.response.use(
  (res) => {
    const body = res.data
    if (body.code !== 0) {
      // 统一错误提示：显示 message，detail 打 console
      window.$message?.error(body.message)
      console.error('[API]', body.detail)
      return Promise.reject(new Error(body.message))
    }
    return body.data            // ★ 直接返回 data，组件里不用层层解包
  },
  (err) => { /* 网络错误 / 401 跳转登录 */ }
)
export default client
```

**布局要求**（参照 `prototype/index.html` 的视觉）：
- 左侧固定侧边栏（可折叠），分组导航
- 顶栏：页面标题 + 面包屑 + 环境标识
- 内容区：路由出口
- 亮色主题（用户 IDE 是 light），但支持暗色切换

**路由表**（与原型 12 个视图对应）：
```ts
const routes = [
  { path: '/', redirect: '/dashboard' },
  { path: '/dashboard',  component: () => import('@/views/Dashboard.vue') },
  { path: '/nodes',      component: () => import('@/views/Nodes.vue') },
  { path: '/clusters',   component: () => import('@/views/Clusters.vue') },
  { path: '/configs',    component: () => import('@/views/Configs.vue') },
  { path: '/deploy',     component: () => import('@/views/Deploy.vue') },
  { path: '/certs',      component: () => import('@/views/Certs.vue') },
  { path: '/backup',     component: () => import('@/views/Backup.vue') },
  { path: '/lvs',        component: () => import('@/views/Lvs.vue') },
  { path: '/logs',       component: () => import('@/views/Logs.vue') },
  { path: '/security',   component: () => import('@/views/Security.vue') },
  { path: '/monitor',    component: () => import('@/views/Monitor.vue') },
  { path: '/build',      component: () => import('@/views/Build.vue') },
  { path: '/audit',      component: () => import('@/views/Audit.vue') },
  { path: '/settings',   component: () => import('@/views/Settings.vue') },
]
```

**验收命令**：
```bash
cd web && pnpm install && pnpm build      # 必须无 TS 错误、无 eslint 错误
pnpm dev                                   # 启动，浏览器打开能看到布局与空页面
```

**陷阱**：
- ⚠️ **Monaco Editor 体积巨大，必须 code splitting 懒加载**，不要放在主 bundle
- ⚠️ 路径别名 `@` 要同时配 `vite.config.ts` 的 `resolve.alias` 和 `tsconfig.json` 的 `paths`

---

## T011 · gRPC Proto 定义与代码生成

**目标**：定义控制面 ↔ Agent 的通信契约（**唯一事实来源**）。

**依赖**：T001

**涉及文件**：
```
proto/agent/v1/agent.proto
gen/agent/v1/agent.pb.go           （生成）
gen/agent/v1/agent_grpc.pb.go      （生成）
internal/agent/transport/grpc_server.go
internal/agent/transport/grpc_client.go
```

**契约**（关键 message，完整定义见 `docs/ARCHITECTURE.md` §4）：

```protobuf
syntax = "proto3";
package agent.v1;

service AgentService {
  // 注册：用一次性 enroll token 换取 mTLS 客户端证书
  rpc Register(RegisterRequest) returns (RegisterResponse);
  // 心跳：双向流，服务端可下发指令
  rpc Heartbeat(stream HeartbeatRequest) returns (stream HeartbeatResponse);
  // 能力上报
  rpc ReportCapability(CapabilityReport) returns (Ack);
}

message RegisterRequest {
  string enroll_token = 1;
  string hostname = 2;
  bytes  csr = 3;              // Agent 本地生成的私钥签名请求，私钥永不出节点
}

message RegisterResponse {
  string node_id = 1;
  bytes  client_cert = 2;      // CA 签发的客户端证书
  bytes  ca_cert = 3;
  int64  cert_expires_at = 4;
  ServerConfig config = 5;     // 心跳间隔、采集配置等
}

message HeartbeatRequest {
  enum Type { PING = 0; CAPABILITY = 1; COMPLIANCE = 2; METRICS = 3; }
  Type type = 1;
  int64 timestamp = 2;         // ★ 节点本地时间，用于检测时钟偏差
  Capability capability = 3;
  ComplianceReport compliance = 4;
  bytes metrics_payload = 5;   // Prometheus 文本格式
}

message HeartbeatResponse {
  enum Command { NONE = 0; REFRESH_CAPABILITY = 1; RUN_COMPLIANCE = 2; }
  Command command = 1;
  string task_id = 2;          // 幂等键
}

message Capability {
  string hostname = 1;
  string os = 2;
  string kernel = 3;
  NginxInfo nginx = 4;         // null 表示非 nginx 节点
  bool has_keepalived = 5;
  bool has_ipvsadm = 6;
  ComplianceReport compliance = 7;
  double clock_skew_seconds = 8;   // 与控制面的时间偏差
}

message NginxInfo {
  string version = 1;
  string binary_path = 2;
  string configure_args = 3;       // nginx -V 原始输出
  string prefix = 4;
  string conf_path = 5;
  string pid_path = 6;
  string error_log = 7;
  string http_log = 8;
  string run_user = 9;
  repeated string static_modules = 10;
  repeated string dynamic_modules = 11;
  repeated ConfigFile config_files = 12;    // ★ 来自 nginx -T
  repeated string log_files = 13;           // ★ 从配置提取
}

message ConfigFile {
  string path = 1;
  string content = 2;
  string sha256 = 3;
  int64  size = 4;
}
```

**验收命令**：
```bash
make proto                      # 生成代码到 gen/
go build ./...                  # 必须无错误
protoc --lint proto/agent/v1/agent.proto    # 风格检查（可选）
```

**陷阱**：
- ⚠️ **Agent 的私钥在节点本地生成，只把 CSR 发给控制面** —— 私钥永不出节点，这是安全底线
- ⚠️ `HeartbeatRequest.timestamp` 是节点本地时间，控制面用它算 `clock_skew_seconds`，偏差 > 1s 要告警
- ⚠️ 所有 RPC 都要带 `task_id` 做幂等，防止重放

---

## T012 · PKI 与 mTLS 证书体系

**目标**：建立 Agent 的 mTLS 身份认证。

**依赖**：T011

**涉及文件**：
```
internal/pkg/pki/ca.go
internal/pkg/pki/issue.go
internal/pkg/pki/verify.go
internal/pkg/pki/ca_test.go
internal/agent/transport/tls.go
scripts/init-pki.sh
```

**契约**：

```go
// CA 初始化：首次启动时自动生成，或加载已有
func LoadOrCreateCA(certPath, keyPath string) (*CA, error)

// 签发 Agent 客户端证书
func (ca *CA) IssueAgentCert(csr []byte, hostname string, ttl time.Duration) (*x509.Certificate, error)

// 证书轮换：到期前 30 天自动续签
func (ca *CA) RenewAgentCert(oldCert *x509.Certificate) (*x509.Certificate, error)

// TLS 配置：服务端要求并校验客户端证书
func (ca *CA) ServerTLSConfig() (*tls.Config, error)
func ClientTLSConfig(caCert, clientCert, clientKey []byte) (*tls.Config, error)
```

**证书设计**：
```
CA (自签, 10 年)
├── CN = ngxcp-agent-ca
└── Agent 证书 (1 年)
    ├── CN = <hostname>
    ├── SAN: DNS:<hostname>          ★ 必须带 SAN，现代 Go/OpenSSL 不看 CN
    ├── ExtKeyUsage: ClientAuth
    └── Serial = <node_id>           ← 控制面从证书里直接取节点身份
```

**关键设计**：**从 TLS 证书的 Serial Number 反查节点身份**，不需要额外的 token。服务端在 `VerifyPeerCertificate` 回调里提取并注入 context。

```go
func (ca *CA) ServerTLSConfig() *tls.Config {
    return &tls.Config{
        ClientAuth: tls.RequireAndVerifyClientCert,
        ClientCAs:  ca.pool,
        VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
            cert := parse(rawCerts[0])
            nodeID := cert.SerialNumber.Int64()
            // 校验节点未被吊销、状态正常
            return ca.validateNode(nodeID)
        },
    }
}
```

**验收命令**：
```bash
bash scripts/init-pki.sh                    # 生成 CA
ls -la /etc/ngxcp/pki/                      # ca.crt (0644) / ca.key (0600)
go test ./internal/pkg/pki/... -v
# 覆盖：签发 → 双向认证握手成功；用其他 CA 签的证书 → 握手失败；过期证书 → 失败
```

**陷阱**：
- ⚠️ **`ca.key` 权限必须 0600**，且**必须纳入备份** —— 丢了所有 Agent 要重新注册
- ⚠️ 必须带 SAN（Go 1.15+ 已废弃 CN-only 校验）
- ⚠️ 证书吊销：节点下线时要加进 CRL 或黑名单表，`VerifyPeerCertificate` 里检查

---

## T013 · HTTP 服务骨架与鉴权

**目标**：控制面的 HTTP 服务、路由、中间件。

**依赖**：T004, T005, T006

**涉及文件**：
```
internal/server/server.go
internal/server/router.go
internal/server/middleware/auth.go
internal/server/middleware/audit.go
internal/server/middleware/logging.go
internal/server/middleware/cors.go
internal/server/handler/health.go
internal/domain/user/user.go              # 本地账号（本任务只做最小可用）
```

**契约**：

```go
func New(cfg *config.Config, db *ent.Client) *Server
func (s *Server) Run(ctx context.Context) error

// 路由分组
v1 := r.Group("/api/v1")
{
    v1.GET("/health", h.Health)
    nodes := v1.Group("/nodes")
    {
        nodes.GET("", h.ListNodes)
        nodes.POST("", h.CreateNode)
        nodes.GET("/:id", h.GetNode)
        nodes.PATCH("/:id", h.UpdateNode)
        nodes.DELETE("/:id", h.DeleteNode)
        nodes.POST("/:id/enroll-token", h.IssueEnrollToken)
        nodes.GET("/:id/capability", h.GetCapability)
        nodes.POST("/:id/refresh", h.RefreshCapability)
    }
    // 后续里程碑追加：/configs /deploy /certs /lvs /logs /security /monitor /build /audit
}
```

**中间件顺序**（很重要）：
```
gin.Recovery()  →  RequestID  →  Logging  →  Auth  →  Audit  →  Handler
```

**鉴权（本任务做最小版本）**：
```go
// 本地账号 + Session（JWT 或服务端 session）
// 密码 argon2id 存储
// 三个角色：admin / operator / viewer
// 登录失败 5 次锁定 15 分钟
// TOTP 二步验证留到 M9 做（本任务先用密码）
```

**审计中间件**：所有非 GET 请求记录到 `audit_log` 表（append-only）
```go
type AuditLog struct {
    UserID, Action, Resource, ResourceID, Detail, IP, UserAgent, CreatedAt
}
```

**验收命令**：
```bash
go run ./cmd/ngxcp-server &
curl -s http://localhost:8080/api/v1/health | jq
# 期望：{"code":0,"data":{"status":"ok","version":"dev"}}

curl -s http://localhost:8080/api/v1/nodes | jq
# 期望：{"code":0,"data":{"items":[],"total":0}}

# 未登录访问写接口
curl -s -X POST http://localhost:8080/api/v1/nodes -d '{}' | jq
# 期望：401
```

**陷阱**：
- ⚠️ 审计中间件只记录**成功的写操作**（失败的记日志即可，否则审计表被刷屏）
- ⚠️ `RequestID` 要贯穿到日志、响应头、审计记录

---

## T014 · Agent 注册与一次性令牌

**目标**：Agent 用一行命令接入控制面。

**依赖**：T012, T013

**涉及文件**：
```
internal/domain/node/enroll.go
internal/server/handler/node.go
internal/agent/cmd/enroll.go
cmd/ngxcp-agent/main.go
scripts/install-agent.sh
```

**契约**：

```go
// 生成一次性令牌（平台 UI 上点"添加节点"生成，或 CLI 生成）
func (s *NodeService) IssueEnrollToken(ctx context.Context, name string, ttl time.Duration) (string, error)
// 令牌格式：ngxcp_<32字节base62>.<校验和>
// 存数据库时只存 SHA256，原文只在生成时返回一次

// Agent 侧注册流程
func (a *Agent) Enroll(ctx context.Context, token, serverAddr string) error {
    // 1. 本地生成私钥 + CSR（私钥写 /etc/ngxcp/agent.key，0600）
    // 2. gRPC Register(token, hostname, csr) —— 首次连接用 TLS 但跳过客户端证书校验
    // 3. 收到客户端证书写入 /etc/ngxcp/agent.crt
    // 4. 保存 serverAddr 到配置文件
}
```

**注册端点要单独开**（因为此时 Agent 还没有客户端证书）：
```go
// 单独的 gRPC 端口或用 TLS 的 InsecureSkipVerify + token 校验
// 推荐：同一个端口，但 Register 方法通过 grpc 拦截器跳过客户端证书校验，
//        改由 enroll_token 鉴权，且令牌一次性 + 有过期时间
```

**接入命令**（目标体验）：
```bash
# 平台生成后给一条命令
curl -fsSL http://ngxcp.internal/api/v1/install.sh | sudo bash -s -- \
  --token ngxcp_xxxxx --server ngxcp.internal:9443
```

或手动：
```bash
sudo ./ngxcp-agent enroll --server ngxcp.internal:9443 --token ngxcp_xxxxx
sudo systemctl enable --now ngxcp-agent
```

**验收命令**：
```bash
# 1. 生成令牌
curl -s -X POST http://localhost:8080/api/v1/nodes/enroll-token \
     -H "Authorization: Bearer $TOKEN" -d '{"name":"rs-nginx-01","ttl":"1h"}' | jq
# 期望：{"code":0,"data":{"token":"ngxcp_...","expires_at":"..."}}

# 2. 起一个测试 Agent（Docker 里跑 nginx）
docker run -d --name test-nginx nginx:1.25-alpine
docker cp ./bin/ngxcp-agent test-nginx:/usr/local/bin/
docker exec test-nginx ngxcp-agent enroll --server host.docker.internal:9443 --token ngxcp_xxx

# 3. 验证节点已注册
curl -s http://localhost:8080/api/v1/nodes | jq '.data.items[] | {name,status,role}'
# 期望：出现测试节点，status=enrolling（等能力上报后转 online）

# 4. 令牌重放必须失败
docker exec test-nginx ngxcp-agent enroll --server ... --token ngxcp_xxx
# 期望：错误 "token already used"
```

**陷阱**：
- ⚠️ **令牌必须一次性** —— 数据库里标记 `used_at`，重放直接拒绝
- ⚠️ **令牌只存 SHA256**，原文不落库（泄露数据库也不会被冒用）
- ⚠️ Agent 私钥文件权限 **0600**，属主 root
- ⚠️ 注册接口虽然跳过客户端证书校验，但**必须用 TLS**（防令牌被中间人截获）

---

## T015 · Agent 心跳与会话管理

**目标**：维持长连接，实时感知节点上下线。

**依赖**：T014

**涉及文件**：
```
internal/agent/session/manager.go
internal/agent/session/heartbeat.go
internal/agent/transport/grpc_server.go     # 实现 Heartbeat 双向流
internal/agent/heartbeat.go                 # Agent 侧
internal/domain/node/status.go
```

**契约**：

```go
// 控制面：会话管理器
type SessionManager struct {
    sessions map[int]*Session       // nodeID -> session
    mu       sync.RWMutex
}

type Session struct {
    NodeID      int
    Stream      agentv1.AgentService_HeartbeatServer
    LastSeen    time.Time
    ClockSkew   time.Duration       // 节点与控制面的时间偏差
    CmdCh       chan *agentv1.HeartbeatResponse
}

func (m *SessionManager) Register(nodeID int, s *Session)
func (m *SessionManager) Unregister(nodeID int)
func (m *SessionManager) Send(nodeID int, cmd *agentv1.HeartbeatResponse) error
func (m *SessionManager) IsOnline(nodeID int) bool
func (m *SessionManager) ClockSkew(nodeID int) time.Duration
```

**心跳参数**：
```yaml
agent:
  heartbeat_interval: 10s
  heartbeat_timeout: 30s         # 超过 30s 没心跳 → 标记 offline
  reconnect_base: 1s
  reconnect_max: 60s             # 指数退避上限
  clock_skew_warn: 1s            # 时间偏差超过 1s 告警
```

**状态机**：
```
enrolling --(能力上报成功)--> online --(30s无心跳)--> offline
   |                            |
   +--(注册后5分钟未上报)--> failed     +--(合规不通过)--> degraded
```

**验收命令**：
```bash
# 起 2 个测试 Agent（docker compose 起两个 nginx 容器）
docker compose -f test/docker-compose.agents.yml up -d
sleep 15
curl -s http://localhost:8080/api/v1/nodes | jq -r '.data.items[] | "\(.name) \(.status)"'
# 期望：两个节点都是 online

# 停掉一个
docker stop test-nginx-01
sleep 35
curl -s http://localhost:8080/api/v1/nodes | jq -r '.data.items[] | "\(.name) \(.status)"'
# 期望：test-nginx-01 变 offline（30 秒内）

# 时间偏差检测
docker exec test-nginx-02 date -s "+1 hour"      # 人为制造偏差
sleep 15
curl -s http://localhost:8080/api/v1/nodes/2 | jq '.data.clock_skew_seconds'
# 期望：约 3600，且平台产生一条 WARN
```

**陷阱**：
- ⚠️ **goroutine 泄漏**：每个 session 有读写两个 goroutine，断开时必须都退出（用 ctx cancel + select）
- ⚠️ **写流并发安全**：`SendMsg` 不能并发调用，必须加锁或用单写 goroutine 消费 `CmdCh`
- ⚠️ **断线重连要指数退避**（1s → 2s → 4s ... → 60s 封顶），否则网络恢复瞬间所有 Agent 一起冲垮控制面
- ⚠️ 心跳超时判定要用**控制面本地时间**，不要用 Agent 上报的时间戳（否则时钟歪的节点永远在线）

---

## T016 · 能力发现（一）：nginx -V 解析 ★

**目标**：解析 nginx 编译参数，得到能力基线。

**依赖**：T015

**涉及文件**：
```
internal/agent/capability/nginx_v.go
internal/agent/capability/nginx_v_test.go
internal/agent/capability/parse.go
testdata/nginx_V_1.30.0.txt        ★ 用用户真实环境的输出
```

**契约**：

```go
type NginxInfo struct {
    Version        string            // "1.30.0"
    BinaryPath     string            // "/usr/sbin/nginx"
    ConfigureArgs  string            // 原始 --prefix=... 整串
    Prefix         string
    ConfPath       string
    SbinPath       string
    PidPath        string
    LockPath       string
    ErrorLogPath   string
    HTTPLogPath    string
    RunUser        string
    RunGroup       string
    Compiler       string
    OpenSSLVersion string
    TLSSNI         bool
    StaticModules  []string          // 从 --with-xxx_module 提取
    DynamicModules []string          // 从配置里的 load_module 补（T017 做）
    ConfigHash     string
}

// 从 `nginx -V` 输出解析（注意：-V 输出到 stderr！）
func ParseNginxV(output string) (*NginxInfo, error)
```

**解析要点**：

```go
// 1. 版本行：nginx version: nginx/1.30.0
var reVersion = regexp.MustCompile(`nginx version: nginx/([\d.]+)`)

// 2. configure arguments: 行后面全是参数
var reConfigure = regexp.MustCompile(`configure arguments:\s*(.+)`)

// 3. 参数解析：--key=value 或 --key（布尔）
//    prefix/sbin-path/conf-path/pid-path/lock-path/error-log-path/http-log-path/user/group

// 4. 模块提取：
//    --with-http_ssl_module     → static_modules: ["http_ssl"]
//    --with-http_v3_module      → ["http_v3"]
//    --with-stream              → ["stream"]
//    --with-stream_ssl_preread_module → ["stream_ssl_preread"]
//    --add-module=../nginx_upstream_check_module
//                               → 取路径末段 "nginx_upstream_check_module"  ★
//    --add-dynamic-module=...   → dynamic_modules

// 5. built with OpenSSL 3.5.1 1 Jul 2025
var reOpenSSL = regexp.MustCompile(`built with OpenSSL ([\d.]+)`)
```

**验收命令**：
```bash
go test ./internal/agent/capability/... -run TestParseNginxV -v
# 必须用 testdata/nginx_V_1.30.0.txt（用户真实输出）作为用例，断言：
#   Version == "1.30.0"
#   Prefix == "/etc/nginx"
#   ConfPath == "/etc/nginx/nginx.conf"
#   SbinPath == "/usr/sbin/nginx"
#   RunUser == "nginx"
#   OpenSSLVersion == "3.5.1"
#   TLSSNI == true
#   StaticModules 含 http_ssl / http_v2 / http_v3 / http_realip / http_stub_status
#                    / http_gzip_static / stream / stream_ssl / stream_ssl_preread
#                    / nginx_upstream_check_module
```

**陷阱** ⚠️：
- **`nginx -V` 输出到 stderr，不是 stdout！** 必须合并 `CombinedOutput()`
- `--add-module=../nginx_upstream_check_module` 的路径是**编译机上的相对路径**，运行时无意义，**取路径末段**提取模块名
- 第三方模块名要归一化（去掉 `nginx_` 前缀、统一 `_module` 后缀），便于与 `configs/module_matrix.json` 比对
- 没有 nginx 的节点（如 Director）要返回 `nil, ErrNginxNotFound`，不要报错

---

## T017 · 能力发现（二）：nginx -T 配置树解析 ★★

**目标**：用 `nginx -T` 得到完整配置树，这是配置中心的地基。

**依赖**：T016

**涉及文件**：
```
internal/agent/capability/config_tree.go
internal/agent/capability/config_tree_test.go
internal/pkg/nginxconf/parser.go
testdata/nginx_T_dump.txt
```

**契约**：

```go
type ConfigFile struct {
    Path     string   // /etc/nginx/nginx.conf
    Content  string
    SHA256   string
    Size     int64
    ModTime  time.Time
}

// ★ 核心：nginx -T 输出自带文件边界标记
//   "# configuration file /etc/nginx/nginx.conf:"
func ParseConfigTree(dump string) ([]ConfigFile, error)

// Agent 侧执行
func (a *Agent) DumpConfig(ctx context.Context) ([]ConfigFile, error) {
    out, err := a.exec(ctx, a.nginxPath, "-T")     // -T 输出到 stdout
    // 配置有语法错误时 -T 会失败 —— 这是特性不是 bug
    if err != nil {
        return nil, apperr.Wrap(apperr.CodePrecondition, "配置 dump 失败", err).
            WithDetail(extractNginxError(out))
    }
    return ParseConfigTree(string(out))
}
```

**解析实现**：

```go
var reConfFile = regexp.MustCompile(`(?m)^# configuration file (.+):$`)

func ParseConfigTree(dump string) ([]ConfigFile, error) {
    matches := reConfFile.FindAllStringSubmatchIndex(dump, -1)
    if len(matches) == 0 {
        return nil, errors.New("未找到配置文件边界标记，可能 nginx 版本不支持 -T")
    }
    var files []ConfigFile
    for i, m := range matches {
        path := dump[m[2]:m[3]]
        start := m[1]
        end := len(dump)
        if i+1 < len(matches) { end = matches[i+1][0] }
        content := strings.TrimSpace(dump[start:end])
        files = append(files, ConfigFile{
            Path:    path,
            Content: content,
            SHA256:  sha256hex(content),
            Size:    int64(len(content)),
        })
    }
    return files, nil
}
```

**验收命令**：
```bash
go test ./internal/agent/capability/... -run TestParseConfigTree -v
# 用例：testdata/nginx_T_dump.txt
# 断言：
#   - 文件数量 == 期望值
#   - 每个文件 path 正确（含 conf.d/ 下的被 include 文件）
#   - content 不含下一个文件的边界标记行
#   - SHA256 稳定（同内容同哈希）

# 边界用例：
#   - 空输出 → 报错
#   - 无边界标记 → 报错
#   - 单个文件 → 正常
#   - 路径含空格 → 正常
```

**陷阱** ⚠️：
- **`nginx -T` 输出到 stdout**，而 `-V` 输出到 stderr —— 不要搞混
- **配置有语法错误时 `-T` 会失败**：这是**特性**，用来检出坏配置。要区分两种失败：
  - `nginx: [emerg] unknown directive` → 语法错误，节点标记 `config_invalid`
  - `nginx: [error] open() "..." failed` → include 文件不存在，同样阻断发布
- **不要读取 `ssl_certificate_key` 指向的文件内容**，只记录路径
- 输出可能几 MB，注意内存与 gRPC 消息大小限制（默认 4MB，要调大 `MaxCallRecvMsgSize`）

---

## T018 · 能力发现（三）：日志路径与文件系统探测

**目标**：确定采集目标，并检查采集的可行性前提。

**依赖**：T017

**涉及文件**：
```
internal/agent/capability/logs.go
internal/agent/capability/logs_test.go
internal/agent/capability/system.go
internal/pkg/nginxconf/directives.go
```

**契约**：

```go
type LogTarget struct {
    Path      string   // /var/log/nginx/access.log
    Type      string   // "access" | "error"
    Format    string   // "main" | "json"（如果指定了 format 名）
    IsSyslog  bool     // syslog:server=... → 跳过采集
    IsOff     bool     // access_log off; → 跳过
    Size      int64
    Inode     uint64
}

// 从 nginx -T 的完整输出里提取所有日志指令
func ExtractLogTargets(files []ConfigFile) []LogTarget

type SystemInfo struct {
    OS            string   // "rockylinux 9.4"
    Kernel        string
    NginxManagedBy string  // "systemd" | "manual"
    SELinuxStatus string   // "enforcing" | "permissive" | "disabled"
    UlimitNofile  int
    Timezone      string
    NTPSynced     bool
    ClockSkew     time.Duration
    LogRotateConf string   // /etc/logrotate.d/nginx 是否存在
    DiskFree      map[string]int64   // 分区 → 剩余字节
}

// ★ 原子落盘可行性检查
type AtomicWriteCheck struct {
    ConfDir      string   // /etc/nginx
    StagingDir   string   // /var/lib/ngxcp/staging
    SameDevice   bool     // ★ 同一设备才能 rename 原子
    ConfDeviceID uint64
    StagingDeviceID uint64
}
func CheckAtomicWrite(confDir, stagingDir string) (*AtomicWriteCheck, error)
```

**日志指令的各种形态（都要处理）**：
```nginx
access_log /var/log/nginx/access.log main;
access_log /var/log/nginx/access.log main buffer=32k flush=5s;
access_log /var/log/nginx/api.access.log json;
access_log off;                                    # → IsOff
access_log syslog:server=10.0.1.5:514,facility=local7;   # → IsSyslog
error_log /var/log/nginx/error.log warn;
error_log /var/log/nginx/api.error.log error;
```

**验收命令**：
```bash
go test ./internal/agent/capability/... -run TestExtractLogTargets -v
# 用例覆盖上述 7 种形态，每种断言正确解析

go test ./internal/agent/capability/... -run TestCheckAtomicWrite -v
# 用例：
#   - 同分区（用 /tmp 下两个目录） → SameDevice=true
#   - 跨分区（/tmp vs /dev/shm 或挂载点） → SameDevice=false
```

**陷阱** ⚠️：
- `access_log off;` 和 `syslog:` 都要**跳过采集**，不要把 "off" 当文件名
- 日志路径支持变量（如 `access_log /var/log/nginx/$host.access.log;`）—— 检测到变量时**标记 `HasVariable=true` 并告警**，不做通配展开（展开不可控）
- **原子落盘检查是发布引擎的前提** —— 不同设备必须降级为 "copy + 校验 + 切换" 并加文件锁，同时告警
- SELinux enforcing 时，Agent 写 `/etc/nginx` 可能因为没有正确的 file context 被拒 —— 要检测并提示

---

## T019 · 节点角色识别与 DR 合规自检

**目标**：自动识别节点角色，并执行 LVS-DR 合规巡检。

**依赖**：T016, T017, T018

**涉及文件**：
```
internal/agent/capability/role.go
internal/agent/probe/dr_compliance.go
internal/agent/probe/dr_compliance_test.go
internal/agent/probe/keepalived.go
internal/domain/lvs/compliance.go
```

**契约**：

```go
type NodeRole string
const (
    RoleRealServer    NodeRole = "real_server"      // 只有 nginx
    RoleDirector      NodeRole = "director"          // keepalived + ipvsadm（用户的 2 台）
    RoleDirectorAndRS NodeRole = "director_and_rs"   // 同机部署
    RoleUnknown       NodeRole = "unknown"
)

func DetectRole(c *Capability) NodeRole

type ComplianceReport struct {
    CheckedAt time.Time
    Role      NodeRole
    Items     []ComplianceItem
    Passed    bool
}

type ComplianceItem struct {
    Name     string   // "vip_on_lo"
    Title    string   // "VIP 绑定在 lo 且掩码 /32"
    Passed   bool
    Expected string
    Actual   string
    Severity string   // "critical" | "warning"
    FixCmd   string   // 修复命令建议
}
```

**DR 模式六项硬约束**（详见 `docs/DECISIONS.md` §4.1）：

| # | 检查 | 命令 | 期望 |
| --- | --- | --- | --- |
| 1 | VIP 在 lo 且 /32 | `ip addr show lo` | 含 `<VIP>/32` |
| 2 | ARP 抑制 | `sysctl -n net.ipv4.conf.{all,lo}.arp_ignore` | = 1 |
| 2b | ARP 宣告 | `sysctl -n net.ipv4.conf.{all,lo}.arp_announce` | = 2 |
| 3 | 反向路径过滤 | `sysctl -n net.ipv4.conf.{all,default,eth0}.rp_filter` | = 0 |
| 4 | VIP 不在物理网卡 | `ip addr show <物理网卡>` | **不含** VIP |
| 5 | 端口一致性 | `ipvsadm -Ln` | VS 端口 == RS 端口 |
| 6 | 二层可达 | `arping -c 1 -I <iface> <VIP>` | 有响应 |

**Keepalived 附加检查**（Director 角色）：
```go
// 两台 Director 的配置应「仅」三项不同：
//   state / priority / unicast_src_ip
// 其余差异 → 告警（配置漂移）
func DiffKeepalivedConfig(master, backup string) ([]DiffItem, error)

// 脑裂检测：两台同时上报持 VIP → CRITICAL
// 由控制面做（它能看到两个节点的状态）
```

**验收命令**：
```bash
go test ./internal/agent/probe/... -v
# 用 testdata 里的模拟输出做表驱动测试

# 真机验证（在 RS 上手工破坏配置）
ssh rs-nginx-01 "sysctl -w net.ipv4.conf.all.arp_ignore=0"
sleep 300      # 等巡检周期（5 分钟）
curl -s http://localhost:8080/api/v1/nodes/2 | jq '.data.compliance'
# 期望：arp_ignore 项 passed=false, severity=critical，节点 status=degraded
```

**陷阱** ⚠️：
- **`rp_filter` 严格模式会静默丢包**：DR 回包源 IP 是 VIP，被判为伪造包。这是最难排查的故障之一
- **`arp_ignore`/`arp_announce` 漂移的典型症状是"流量时通时断"** —— 因为不同客户端 ARP 缓存不同
- 检查 `sysctl` 时用 `sysctl -n`（只输出值），不要解析 `sysctl -a` 的输出
- 物理网卡名不要硬编码 `eth0`，从默认路由取（`ip route get 8.8.8.8`）
- **Director 的 vSwitch 端口组安全策略无法从 Guest 内检测** —— 只能做成部署检查清单（见 M5）

---

## T020 · 节点管理页面

**目标**：节点列表、详情、能力基线展示。

**依赖**：T010, T013, T016, T017, T018, T019

**涉及文件**：
```
web/src/views/Nodes.vue
web/src/api/nodes.ts
web/src/components/node/NodeCard.vue
web/src/components/node/CapabilityPanel.vue
web/src/components/node/CompliancePanel.vue
web/src/components/node/EnrollDialog.vue        # 生成接入命令
```

**页面要求**（视觉与交互基准：`prototype/index.html` 的「节点管理」）：

**列表页**：
- 卡片式布局，每张卡显示：节点名、角色徽章、状态灯、地址、nginx 版本、最后心跳
- 顶部筛选：角色 / 状态 / 集群
- 右上角「添加节点」按钮 → EnrollDialog（生成令牌 + 一行命令 + 复制按钮）

**详情页**（抽屉或新页面），分 Tab：
1. **概览** —— 基本信息、状态、时钟偏差、LVS 权重
2. **能力基线** —— `nginx -V` 解析结果：版本、prefix、路径、模块清单（标签云）
3. **配置树** —— 文件列表（路径 + 大小 + 哈希 + 修改时间），可预览内容
4. **日志路径** —— 采集目标清单，标注类型、格式、是否采集
5. **DR 合规** —— 六项检查的结果卡片，不通过项高亮 + 显示修复命令
6. **系统信息** —— OS、内核、SELinux、ulimit、NTP、磁盘、原子落盘检查结果

**验收命令**：
```bash
cd web && pnpm build          # 无 TS 错误
pnpm dev
# 浏览器打开 /nodes：
#   - 能看到 4 个节点（2 director + 2 real_server）
#   - 点开 RS 详情 → 能力基线 Tab 显示 nginx 1.30.0 与完整模块清单
#   - 配置树 Tab 显示 nginx.conf 与 conf.d/ 下的文件
#   - DR 合规 Tab 显示六项检查结果
```

**陷阱**：
- ⚠️ 模块清单可能很长，用标签云 + 折叠
- ⚠️ 配置文件内容可能很大，**默认不加载内容，点击才请求**
- ⚠️ 状态灯颜色：online=绿 / offline=灰 / degraded=黄 / enrolling=蓝

---

## M1 集成验收 ★

```bash
# 1. 起全套
docker compose up -d
make build
go run ./cmd/ngxcp-server &

# 2. 4 个 Agent 接入（2 director + 2 real_server）
#    用 test/docker-compose.agents.yml 起测试容器
docker compose -f test/docker-compose.agents.yml up -d

# 3. 验证
sleep 30
curl -s http://localhost:8080/api/v1/nodes | jq -r '.data.items[] | "\(.name)\t\(.role)\t\(.status)\t\(.capability.nginx.version)"'
# 期望输出类似：
#   director-01   director       online   null
#   director-02   director       online   null
#   rs-nginx-01   real_server    online   1.30.0
#   rs-nginx-02   real_server    online   1.30.0

# 4. 掉线感知
docker stop test-rs-nginx-01 && sleep 35
curl -s http://localhost:8080/api/v1/nodes | jq -r '.data.items[] | "\(.name) \(.status)"' | grep nginx-01
# 期望：offline

# 5. 双机能力一致性
curl -s http://localhost:8080/api/v1/nodes/capability-diff | jq
# 期望：两台 RS 的模块清单完全一致，any_drift=false

# 6. 前端
cd web && pnpm dev     # 浏览器验证节点页与详情页
```

**全部通过后才进入 M2。**
