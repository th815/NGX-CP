# NGX-CP 技术架构设计

v0.1 · 2026-09-03

---

## 1. 架构总览

```
                        ┌───────────────────────────────────────┐
   浏览器 ───HTTPS───▶  │          Web UI (Vue 3 + TS)          │
                        └──────────────────┬────────────────────┘
                                           │ REST / JSON
   CLI / CI ───API────────────────────────▶│
                        ┌──────────────────▼────────────────────┐
                        │         API Server (Go)               │
                        │  ┌─────────────────────────────────┐  │
                        │  │ Auth/RBAC │ Config │ Cert │ LVS  │  │
                        │  ├─────────────────────────────────┤  │
                        │  │ Deploy Engine (状态机 + 批处理)   │  │
                        │  ├─────────────────────────────────┤  │
                        │  │ Scheduler (漂移检测/证书扫描)     │  │
                        │  └─────────────────────────────────┘  │
                        └───┬──────────┬──────────┬─────────┬───┘
                            │          │          │         │
                    ┌───────▼──┐  ┌────▼────┐ ┌───▼───┐ ┌──▼──────┐
                    │ Postgres │  │  Redis  │ │  Git  │ │ 对象存储 │
                    │  元数据  │  │ 队列/锁 │ │配置版本│ │  快照   │
                    └──────────┘  └─────────┘ └───────┘ └─────────┘
                            │
                    gRPC / WebSocket (mTLS)  ← Agent 主动外连
        ┌───────────────────┼───────────────────┬──────────────────┐
        ▼                   ▼                   ▼                  ▼
  ┌──────────┐        ┌──────────┐        ┌──────────┐      ┌──────────┐
  │  Agent   │        │  Agent   │        │  Agent   │      │  Agent   │
  │ Nginx    │        │ Nginx    │        │ LVS Dir  │      │ LVS Dir  │
  │ Node-01  │        │ Node-02  │        │ (keepal.)│      │ (keepal.)│
  └──────────┘        └──────────┘        └──────────┘      └──────────┘
```

**关键设计：Agent 主动外连，控制面永不主动连节点。**

理由：
- 跨机房 / NAT / 云上云下混合场景无需开 SSH 端口、无需互通网络。
- 只需 Agent 能访问控制面 443 端口，防火墙策略极简。
- 节点上下线自感知，控制面无状态水平扩展。

---

## 2. 技术选型

| 层 | 选型 | 理由 |
| --- | --- | --- |
| 前端 | **Vue 3 + TypeScript + Naive UI + Monaco Editor** | Monaco 提供 Nginx 语法高亮与 Diff 编辑器，与 VS Code 同内核，运维接受度高 |
| 后端 | **Go 1.22+（Gin 或 Hertz）+ `embed.FS` 内嵌前端** | 单二进制交付；与 Agent 共用 protobuf 定义 |
| Agent | **Go，单二进制 < 20MB** | 零依赖（不要求节点装 Python/Node/rsync/git）；模块含配置下发 + 日志采集 + 合规巡检 |
| 通信 | **gRPC 双向流 + mTLS** | Agent 主动外连，节点无需开放入站端口；断线指数退避重连 |
| 数据库 | **PostgreSQL 16（主库）** | 单机 4 节点写 QPS 个位数；关系查询（配置血缘/审计/证书）是刚需；备份 `pg_dump`+WAL 归档+异地。**开发态可用 SQLite 同构**（sqlc+仓储层抽象，换 driver 即可） |
| 队列/锁 | **进程内 goroutine + SQLite 事务** | 不引入 Redis；Agent 会话表放内存 map；变更锁用乐观锁 `UPDATE ... WHERE` |
| 配置版本 | **SQLite 内容寻址 blob + revision 链路** | 相同内容只存一份（去重）；`parent_id` 串成版本链；读取时用 `gotextdiff` 实时算 diff。<br>**可选**：开关式导出到 Git 裸仓做异地备份（go-git） |
| 快照存储 | **本地目录**（`/var/lib/ngxcp/snapshots/<node>/<ts>.tar.gz`） | 2 节点规模无需对象存储；保留策略 90 天 / 最近 200 份 |
| 日志采集 | **Agent 内置 tail 模块**（默认开） | 少一个要运维的组件；offset 持久化 + 本地磁盘队列（断连补传 24h）+ 采样降载。<br>Vector 作为大流量可选增强 |
| 日志存储 | **ClickHouse 单实例**（`max_memory_usage=2GB`，TTL 7 天） | 压缩比 10:1；**检测规则就是 SQL**，运维零学习成本。<br>控制面仅 2G 内存时改用 VictoriaLogs |
| 告警事件 | **ClickHouse（永久）+ SQLite 镜像** | 体积小，且是安全审计依据 |
| 指标 | **Agent 心跳 + `stub_status` → SQLite 时序表** | Prometheus/Grafana 延后到 v0.3 |
| 部署 | **单二进制 + systemd**（控制面 2C4G / 60GB SSD） | 无 Docker、无 K8s、无中间件依赖 |

> 完整选型权衡见 `docs/DECISIONS.md`。


---

## 3. 通信协议

### 3.1 Agent 注册

```bash
# 控制台生成一次性令牌，节点上执行一条命令即可
curl -fsSL https://ngxcp.internal/api/v1/enroll.sh | \
  NGXCP_TOKEN=eyJhbGciOi... NGXCP_CLUSTER=prod-web sh -
```

注册流程：
1. Agent 携带 token 调用 `Enroll(nodeInfo)`，nodeInfo 含 hostname/ip/nginx_version/os/kernel/ipvs 能力探测结果。
2. 服务端校验 token → 签发**节点证书**（mTLS 客户端证书）→ 返回 `node_id` + CA。
3. Agent 将证书写入 `/var/lib/ngxcp/pki/`，后续所有连接使用 mTLS。

### 3.2 指令流（服务端 → Agent）

```protobuf
service AgentService {
  rpc Connect(stream AgentMessage) returns (stream ServerCommand);
}

message ServerCommand {
  string cmd_id      = 1;
  oneof payload {
    Ping           ping       = 10;
    ConfigPush     push       = 11;  // 下发配置文件（含期望 hash）
    ConfigFetch    fetch      = 12;  // 回读节点当前配置
    NginxTest      test       = 13;  // nginx -t
    NginxReload    reload     = 14;
    NginxRestart   restart    = 15;
    CertPush       cert       = 16;  // 下发证书
    Snapshot       snapshot   = 17;  // 打包备份
    Restore        restore    = 18;  // 恢复快照
    IpvsadmApply   ipvs       = 19;  // 应用 LVS 规则
    KeepalivedApply ka        = 20;  // 应用 keepalived 配置
    DrPrecheck     drcheck    = 21;  // DR 模式合规自检
  }
}
```

### 3.3 Agent 安全边界（硬约束，编译期常量）

```go
var AllowedWritePaths = []string{
    "/etc/nginx/",          // 仅允许写 Nginx 配置目录
    "/etc/keepalived/",     // LVS 配置
    "/etc/ssl/ngxcp/",      // 平台托管的证书目录
}
var AllowedCommands = []string{
    "nginx", "ipvsadm", "ipvsadm-save", "ipvsadm-restore",
    "systemctl", "tar", "curl",
}
```

> Agent 拒绝任何写白名单外路径的指令，且拒绝任意 shell 命令执行 —— **不提供远程命令执行能力**，这是防止 Agent 变成后门的关键。

---

## 4. 发布引擎（核心）

### 4.1 任务状态机

```
                  ┌─────────┐
                  │ pending │
                  └────┬────┘
                       │ 审批通过 / 定时触发
                       ▼
                  ┌─────────┐   用户中止   ┌───────────┐
                  │ running ├────────────▶│ cancelled │
                  └────┬────┘             └───────────┘
                       │
        ┌──────────────┼──────────────┐
        ▼              ▼              ▼
   ┌─────────┐   ┌─────────┐   ┌─────────┐
   │ batch 1 │──▶│ batch 2 │──▶│ batch N │
   └────┬────┘   └────┬────┘   └────┬────┘
        │             │             │
        │ 失败率>阈值  │             │
        └─────────────┴─────────────┘
                       ▼
                 ┌───────────┐
                 │ rolling   │  自动回滚所有已变更批次
                 │   back    │
                 └─────┬─────┘
                       ▼
              ┌────────────────┐
              │ failed/success │
              └────────────────┘
```

### 4.2 单节点执行步骤（原子序列）

```
1. lock     获取节点级变更锁（防并发）
2. snapshot tar czf /etc/nginx → 上传对象存储（失败即中止）
3. push     写入新配置到临时目录 /etc/nginx/.ngxcp-staging/
4. test     nginx -t -c 临时配置（含 -p 指向 staging 目录）
   └─ 失败 → 清理 staging → 标记失败 → 不触碰线上配置
5. swap     staging → 生产（原子 rename，保留旧目录为 .ngxcp-prev）
6. reload   nginx -s reload
7. probe    3s 后 HTTP 探活（配置 URL，默认 / 或 /healthz）
   └─ 失败 → 恢复 .ngxcp-prev → reload → 标记失败
8. verify   回读配置 hash 与期望值比对（防漂移）
9. unlock   释放锁
```

**回滚路径**：`restore .ngxcp-prev` 或 `从快照恢复` → `reload` → 探活。

### 4.3 灰度策略配置示例

```json
{
  "mode": "ratio",                    // ratio | explicit | all
  "batches": [
    { "seq": 1, "ratio": 10, "nodes": [], "observe_seconds": 60 },
    { "seq": 2, "ratio": 50, "nodes": [], "observe_seconds": 120 },
    { "seq": 3, "ratio": 100,"nodes": [], "observe_seconds": 0 }
  ],
  "failure_threshold_ratio": 0,       // 批次内失败率超过即熔断
  "auto_rollback": true,
  "probe": { "url": "/healthz", "expect_status": 200, "timeout_ms": 3000 },
  "concurrency_per_batch": 5          // 批次内并发节点数
}
```

---

## 5. 配置模板与变量

### 5.1 三级变量优先级

```
节点级变量 (node.vars)      ← 优先级最高
    ↓ 覆盖
集群级变量 (cluster.vars)   ← 中间层
    ↓ 覆盖
全局变量   (global.vars)    ← 默认值
```

### 5.2 模板示例

```jinja2
# template: upstream.j2  ——  由平台渲染
upstream {{ app_name }} {
    least_conn;
    {% for rs in upstream_nodes %}
    server {{ rs.ip }}:{{ rs.port | default(8080) }} weight={{ rs.weight | default(10) }}
           max_fails=3 fail_timeout=10s;
    {% endfor }
    keepalive 64;
}
```

> 发布前，平台对**每个节点单独渲染**并在 UI 上展示渲染结果 Diff —— 避免"模板改了但不知道会变成啥"的恐慌。

---

## 6. 证书子系统

```
┌─────────────────────────────────────────────────────┐
│  Cert Manager                                       │
│                                                     │
│  ┌───────────┐   ┌───────────┐   ┌──────────────┐  │
│  │ ACME 签发 │   │ 手动上传  │   │ 自动续期调度 │  │
│  │ (lego库)  │   │ (PEM/PFX) │   │ (每日 03:00) │  │
│  └─────┬─────┘   └─────┬─────┘   └──────┬───────┘  │
│        └───────────────┼────────────────┘          │
│                        ▼                            │
│              ┌──────────────────┐                   │
│              │ 加密存储 (AES-GCM)│                   │
│              │  key 由 KMS/env  │                   │
│              └────────┬─────────┘                   │
│                       ▼                             │
│          ┌────────────────────────┐                 │
│          │ 绑定到集群 → 轻量发布   │                 │
│          │ /etc/ssl/ngxcp/<cn>/   │                 │
│          │   fullchain.pem        │                 │
│          │   privkey.pem (0600)   │                 │
│          └────────────────────────┘                 │
└─────────────────────────────────────────────────────┘
```

**ACME 挑战方式**：
- **DNS-01（默认，也是通配符证书的唯一选择）**：当前 DNS 在 Cloudflare，且 CF 前置会拦截 HTTP-01 的验证请求，因此**直接上 DNS-01**。
- **HTTP-01**：仅在没有 DNS API 权限时兜底（平台自动下发临时 location，验证后清理）。

**DNS Provider 抽象**（DNS 从阿里云迁至 Cloudflare，未来可能再变）：

```go
type DNSProvider interface {
    Name() string
    SetRecord(ctx context.Context, zone, name, value string) error  // _acme-challenge TXT
    DeleteRecord(ctx context.Context, zone, name string) error
    Validate() error                                                 // 测试 API 凭据
}
```

v1 实现 `Cloudflare`，预留 `AliyunDNS`（历史域名仍在）、`DNSPod`、`Route53`。加一个 provider 约 100 行。

**Cloudflare API Token 最小权限**：`Zone.Zone:Read` + `Zone.DNS:Edit`，**限定到具体 zone**。Token 加密存 SQLite，主密钥取 `NGXCP_MASTER_KEY` 环境变量或 `/etc/ngxcp/master.key`（0600）。

**手动上传的 6 项校验**（任一不过即拒绝）：

| 检查 | 说明 |
| --- | --- |
| 私钥/证书匹配 | 比对公钥模数，不匹配直接拒绝 |
| 链完整性 | 缺 intermediate 会导致部分客户端（Android/Java）不信任 |
| 链顺序 | 必须 leaf → intermediate，**不得含 root** |
| 域名覆盖 | SAN 是否覆盖所有引用它的 `server_name` |
| 有效期 | 已过期或剩余 < 7 天标红 |
| 签名算法 | SHA-1 / MD5 直接拒绝 |

**分发与安全红线**：

```
Agent 拉取（mTLS）→ 内存解密 → 写 /etc/nginx/ssl/<domain>.{crt,key}
  → chmod 0644 crt / 0600 key → chown root:root
  → 原子 rename → nginx -t → reload → 探活
```

- 私钥**永不下发浏览器**（API 只返回元数据：域名/签发者/到期/指纹/算法）
- 下载私钥需二次确认 + 审计留痕
- 私钥存独立 `certificate` 表，**不进 `config_blob` 版本历史**

**自动续期**：到期前 30 天起每日 03:00 检查 → 续期成功则创建 `source='cert-renew'` 变更单走完整发布流水线 → 连续 3 天失败升级为 CRITICAL。注意 Let's Encrypt 限流（每周每域名 50 张），平台侧做本地限流避免调试时自锁。

**Nginx 1.30 相关**：

```nginx
ssl_certificate     /etc/nginx/ssl/example.com.crt;   # 静态路径 + reload
ssl_certificate_key /etc/nginx/ssl/example.com.key;   # 避免用变量（每次握手读盘）
listen 443 quic reuseport;                             # 编译了 --with-http_v3_module
listen 443 ssl;
http2 on;
add_header Alt-Svc 'h3=":443"; ma=86400' always;
```

---

## 7. LVS 子系统

### 7.1 模型映射

| 平台概念 | 实际产物 |
| --- | --- |
| LvsVirtualService | `ipvsadm -A -t <VIP>:<port> -s <scheduler>` |
| LvsRealServer | `ipvsadm -a -t <VIP>:<port> -r <RS_IP>:<port> -g -w <weight>` |
| 摘除 RS | `ipvsadm -e ... -w 0`（平滑）或 `-d`（立即） |
| Keepalived 配置 | `/etc/keepalived/keepalived.conf` 全量渲染下发 |
| 健康检查 | 复用 Nginx 节点自身 `/healthz`，或 keepalived `TCP_CHECK`/`HTTP_GET` |

### 7.2 DR 模式合规自检（Agent 每 5 分钟巡检）

六个硬约束，任一漂移 → 节点标红 → **阻断该节点参与 LVS 发布**（不断业务，只是不让发）：

```bash
# 1. VIP 绑在 lo 上，掩码必须是 /32
ip addr show lo | grep -q "<VIP>/32" || FAIL "VIP not bound to lo:0 (/32)"

# 2. ARP 抑制（最关键：违反会导致流量时通时断，最难排查）
sysctl -n net.ipv4.conf.all.arp_ignore   | grep -qx "1" || FAIL
sysctl -n net.ipv4.conf.all.arp_announce | grep -qx "2" || FAIL
sysctl -n net.ipv4.conf.lo.arp_ignore    | grep -qx "1" || FAIL
sysctl -n net.ipv4.conf.lo.arp_announce  | grep -qx "2" || FAIL

# 3. 反向路径过滤必须关闭（严格模式下回包源 IP 是 VIP 会被内核丢弃）
sysctl -n net.ipv4.conf.all.rp_filter    | grep -qx "0" || FAIL
sysctl -n net.ipv4.conf.default.rp_filter| grep -qx "0" || FAIL
sysctl -n net.ipv4.conf.eth0.rp_filter   | grep -qx "0" || FAIL

# 4. VIP 绝不能出现在物理网卡上
ip addr show eth0 | grep -q "<VIP>" && FAIL "VIP must not be on eth0"

# 5. 端口一致性（DR 不支持端口映射）：VIP:80 → RS:80
ipvsadm -Ln | awk 校验 VS 端口 == RS 端口 || FAIL

# 6. 二层可达性：Director 与 RS 同网段
#    由控制面按节点 IP 与 VIP 网段静态校验 + Agent 侧 arping 探测
```

**Keepalived 主备额外检查项**：

| 项 | 检查内容 |
| --- | --- |
| `virtual_router_id` | 同二层域内唯一（0–255） |
| 单播配置 | 云厂商 VPC 禁 VRRP 组播 → 必须 `unicast_src_ip` + `unicast_peer` |
| 健康检查 | `vrrp_script` 是否存在且能检出 nginx 故障 |
| 脑裂探测 | 两节点同时上报持 VIP → 立即 CRITICAL 告警 |
| 双机一致性 | 两台 Director 配置应**仅** `state` / `priority` / `unicast_src_ip` 三项不同，其余差异标红 |

**Keepalived 2.x 注意**：AH 认证已被移除，只剩 `auth_type PASS`（明文），安全性靠网络隔离，不要指望它。

不合规 → 节点详情页红色徽标 → **阻断该节点参与 LVS 发布**。

### 7.3 LVS 变更执行序列

```
1. ipvsadm-save > /var/lib/ngxcp/backup/ipvs.<ts>.rules     （先备份）
2. cp keepalived.conf → 备份
3. 应用增量 ipvsadm 命令 或 全量渲染 keepalived.conf
4. keepalived -t -f keepalived.conf                          （配置校验）
5. systemctl reload keepalived
6. ipvsadm -Ln --stats 回读校验实际生效规则与期望一致
7. 不一致 → ipvsadm-restore < 备份 → 标记失败
```

---

### 7.4 无损发布序列（LVS 权重摘除式）

2 节点场景下，真正的灰度不是 Nginx 层的百分比，而是 **LVS 层的权重摘除**。这是本平台相对"通用配置分发工具"的核心差异点：

```
发布 RS2（nginx-02）的完整序列：

  1. ipvsadm -e -t VIP:80 -r RS2:80 -g -w 0        # 权重设 0，新连接不再调度过来
  2. 等待活跃连接排空                                 # 轮询 Established 数归零，最多 60s
  3. 下发新配置 → SHA256 校验 → nginx -t → 原子落盘 → reload
  4. 双层探活：
       - 本地   curl -sf http://127.0.0.1/health
       - Director 侧 curl -sf -H 'Host: x' --resolve ... http://RS2/health
  5. ipvsadm -e -t VIP:80 -r RS2:80 -g -w 1        # 加回权重
  6. 观测 60s（5xx 率 / 响应时间 / check 模块健康状态 / 系统负载）
  7. 正常 → 对 RS1 重复 1–6
     异常 → 立即回滚 RS2（快照恢复 + reload + 恢复权重）并中止整个任务
```

**要点**：RS 摘除期间 LVS 不会把新连接调度过去，因此整个变更过程**用户侧零 5xx**。纯 Nginx 层的 reload 做不到这一点（reload 瞬间可能有连接被 reset，虽然 Nginx 的 graceful shutdown 已缓解，但长连接仍会受影响）。

### 7.5 两层健康检查的分工（不要混为一谈）

| 层 | 检查者 | 检查对象 | 失效后果 | 告警级别 |
| --- | --- | --- | --- | --- |
| **LVS 层** | keepalived `TCP_CHECK` / `HTTP_GET` | RS（Nginx 节点）是否存活 | 权重设 0，LVS 不再调度 | RS 全 down = **致命**（业务中断） |
| **Nginx 层** | `nginx_upstream_check_module` 的 `check` 指令 | upstream server（后端应用）是否存活 | 从 upstream 摘除，不再转发 | 单个 down = **降级**（还有别的后端） |

平台两层都可视化，且**告警语义必须区分**。

---

## 8. 节点能力基线（`nginx -V`）

纳管节点时执行 `nginx -V` 并解析为结构化画像，作为配置校验的"能力边界"：

```json
{
  "version":   "1.30.0",
  "prefix":    "/etc/nginx",
  "conf_path": "/etc/nginx/nginx.conf",
  "sbin_path": "/usr/sbin/nginx",
  "modules":  ["http_ssl","http_v2","http_v3","realip","stub_status",
               "gzip_static","stream","stream_ssl","stream_ssl_preread",
               "nginx_upstream_check_module"],
  "raw_args": "--prefix=/etc/nginx --sbin-path=/usr/sbin/nginx ..."
}
```

**三个用途**：

**① 校验时按能力检查** —— 配置里出现 `check interval=3000` 但目标节点未编译 `nginx_upstream_check_module` → **校验阶段即失败**，而不是等下发后 `nginx -t` 报错再回滚。

**② 双机编译一致性检测** —— nginx-01 与 nginx-02 的 `nginx -V` **应当完全一致**。

> 这是 2 节点环境的高频坑：在一台上重新编译加了模块，忘了另一台，配置同步过去直接 `nginx -t` 失败。平台自动 diff 两台的模块清单，差异高亮告警。

**③ 配置块支持范围** —— 因为编译了 `--with-stream`，平台**必须支持 `stream{}` 块**（大量工具只管 `http{}`）：

```
nginx.conf
├── events { }
├── http   { }   ← server / upstream / location
│   └── conf.d/*.conf
└── stream { }   ← 四层代理（配合 ssl_preread 做基于 SNI 的分流，不解密）
    └── stream.d/*.conf
```

---

## 9. 日志与安全子系统

### 9.1 要解决的三个断裂

Nginx 集群化后排查难，本质是：

1. **位置断裂** —— 请求散落在不同节点，不知道该去哪台查
2. **格式断裂** —— 各节点格式不统一，查到了也对不齐
3. **关联断裂** —— 同一会话的多段日志串不起来

### 9.2 第 0 层：TraceID 透传（零成本，收益最大）

```nginx
log_format ngxcp escape=json '{"rid":"$request_id", ... }';
add_header      X-Request-Id $request_id always;   # 回写客户端
proxy_set_header X-Request-Id $request_id;         # 透传后端
```

**效果**：用户报障时从响应头复制 `X-Request-Id`，平台上一次检索即可定位"打在哪个节点 → 转发到哪个 upstream → 各段耗时"。

**DR 模式的天然优势**：DR 下 RS 收到的包源 IP 就是真实客户端 IP，所以 `$remote_addr` 直接可用，无需 realip 处理 VIP 干扰。但若前面还有 CDN：

```nginx
set_real_ip_from <Cloudflare IP 段>;
real_ip_header   CF-Connecting-IP;
```

> 平台提供「更新 CDN IP 段」按钮：自动拉取 `https://api.cloudflare.com/client/v4/ips` 生成 `set_real_ip_from` 片段，并纳入版本管理（Cloudflare 的 IP 段会变，需定期更新）。

### 9.3 第 1 层：标准日志格式（平台一键下发）

```nginx
log_format ngxcp escape=json '{'
    '"ts":"$time_iso8601","rid":"$request_id","node":"$hostname",'
    '"host":"$host","ip":"$remote_addr","xff":"$http_x_forwarded_for",'
    '"method":"$request_method","uri":"$request_uri","proto":"$server_protocol",'
    '"status":$status,"bytes":$body_bytes_sent,"rt":$request_time,'
    '"urt":$upstream_response_time,"uaddr":"$upstream_addr",'
    '"ustatus":"$upstream_status","uct":$upstream_connect_time,'
    '"referer":"$http_referer","ua":"$http_user_agent",'
    '"ssl_p":"$ssl_protocol","ssl_c":"$ssl_cipher"'
'}';
```

**三个排查命门字段**：`upstream_addr`（落在哪个后端，多值 = 发生重试）、`upstream_response_time`（慢在后端还是 Nginx，对比 `request_time`）、`upstream_status`（502 时后端到底返回了什么）。

`error.log` 同样要采集 —— SSL 握手失败、upstream 连接拒绝、`limit_req` 触发都只出现在那里。

### 9.4 第 2 层：Agent 内置采集

| 方案 | 内存 | 结论 |
| --- | --- | --- |
| **Agent 内置 tail** | 0（复用进程） | ✅ 默认。少一个要运维/升级/监控的组件 |
| Vector | ~50MB | 🟡 大流量时的可选增强 |
| Fluent Bit | ~20MB | 🟡 配置语法自成一套 |
| Filebeat | ~100MB+ | ❌ 2 节点没必要 |

**实现要点**：

- 独立 goroutine + `defer recover()`，**采集失败绝不影响配置下发主流程**
- offset 持久化（重启不丢不重）
- 本地磁盘队列（控制面挂了先攒，默认 500MB 或 24h，恢复后补传）
- 批量压缩上传（1000 条 或 5 秒 触发）
- 采样降载：状态码 ≥ 500 全采；2xx/3xx 按比例采样（日志量大时救磁盘）

### 9.5 第 3 层：ClickHouse 存储

```sql
CREATE TABLE nginx_access (
    ts      DateTime64(3),
    node    LowCardinality(String),
    rid     String,
    host    LowCardinality(String),
    ip      IPv4,                      -- IPv4 类型比 String 省一半空间
    method  LowCardinality(String),
    uri     String,
    status  UInt16,
    bytes   UInt32,
    rt      Float32,
    urt     Float32,
    uaddr   String,
    ustatus String,
    ua      String,
    referer String
) ENGINE = MergeTree
  PARTITION BY toYYYYMMDD(ts)
  ORDER BY (ts, node, status)
  TTL ts + INTERVAL 7 DAY;             -- 全量日志保留 7 天，可配

-- 告警事件永久保留（体积小，且是安全审计依据）
CREATE TABLE security_event (
    ts       DateTime64(3),
    rule     LowCardinality(String),
    level    LowCardinality(String),   -- info | warn | critical
    src_ip   IPv4,
    node     LowCardinality(String),
    hits     UInt32,
    window   UInt32,
    evidence String,                   -- 抽样样本 JSON
    action   LowCardinality(String),   -- none | pending-review | blocked
    change_id Nullable(Int64)          -- 关联的封禁变更单
) ENGINE = MergeTree
  PARTITION BY toYYYYMM(ts)
  ORDER BY (ts, rule);
```

**选 ClickHouse 的决定性理由**：检测规则本质就是 SQL 聚合，运维零学习成本。若控制面只有 2G 内存，改用 VictoriaLogs（~500MB，LogsQL）。

### 9.6 第 4 层：攻击检测规则

滑动窗口统计（1 分钟 / 5 分钟 / 1 小时）：

| 规则 | 判据 | 级别 | 默认动作 | 误报 |
| --- | --- | --- | --- | --- |
| 注入特征 | URI/UA/Referer 正则命中 SQLi / XSS / 命令注入 / 路径穿越 | CRITICAL | 自动封禁 | 低 |
| 扫描器指纹 | UA 匹配 `sqlmap\|nmap\|nikto\|masscan\|nessus\|acunetix\|dirbuster` | CRITICAL | 自动封禁 | 极低 |
| 目录爆破 | 单 IP 60s 内 `404 ≥ 50` 且 **去重 URI ≥ 30** | CRITICAL | 生成变更单，待审批 | 中 |
| CC 洪水 | 单 IP 60s 请求数 > `max(600, 7天基线均值 × 8)` | CRITICAL | 生成变更单，待审批 | 中（移动 NAT 出口） |
| 敏感路径探测 | `/.env` `/.git` `/wp-login` `/phpmyadmin` `/actuator` `/adminer` | WARN | 仅记录 | 高 |
| 慢速攻击 | `rt > 30s` 且并发连接数异常（Slowloris） | WARN | 仅记录 | 中 |
| 5xx 突增 | 5 分钟 5xx 率 > 5% 且环比 +300% | CRITICAL | 告警（**故障不是攻击**） | 低 |
| 4xx 突增 | 403/404 率环比 +200% | INFO | 仅记录（可能是刚发的配置写错） | 中 |
| 异常地理/ASN | GeoIP 命中非业务地区且高频 | INFO | 仅记录 | 高 |
| SSL 握手失败潮 | error.log handshake failure 激增 | WARN | 仅记录 | 中 |

### 9.7 封禁联动：复用发布流水线（关键设计）

**绝不为封禁新开一条写线上的通道。** 任何绕过发布流水线的"便捷通道"，最终都会成为事故来源。

```
命中规则
  → 生成 deny 片段（追加到 /etc/nginx/conf.d/zz-blocklist.conf）
  → 创建配置变更单（source = 'security'）
  → 走完整的 校验 → 灰度 → 探活 → 回滚 链路
  → 全程审计留痕
```

收益：封禁**可回滚**（自动封禁误伤时一键撤回，不用去机器上删行）、**可灰度**（先 1 台生效观察）、**可审批**（高误报规则默认人工确认）、**零新增机制**（复用配置中心 + 发布引擎 + 审计）。

**分级处置策略（按规则可配）**：

| 模式 | 触发条件 | 行为 |
| --- | --- | --- |
| 自动 | CRITICAL + 高置信（注入特征、扫描器指纹） | 生成变更单 → 自动执行 → 通知 |
| 半自动 | CRITICAL + 中置信（CC、目录爆破） | 生成变更单 → 等审批 → 通知 |
| 仅告警 | WARN / INFO | 只记录，告警页可手动「一键封禁」 |

### 9.8 检索交互要求

- 时间快选：5m / 1h / 24h / 7d / 自定义
- 筛选：节点 · 状态码 · 方法 · URI（精确/前缀/正则）· IP · `request_id` · `rt > N` · upstream
- **TraceID 追踪**：输入 ID → 展示完整链路（哪个节点 → 哪个 upstream → 各段耗时）
- 聚合视图：Top URI / Top IP / Top UA / 状态码分布 / P50·P95·P99 曲线
- 保存的查询（常用排查场景存为快捷方式）
- 表格展开看完整 JSON

### 9.9 轻量降级方案

不想上 ClickHouse 时：

- **Agent 本地实时检测** —— 节点上滑动窗口统计，只上报命中的告警事件到控制面（存 SQLite）
- **全量日志不落中心** —— 各节点本地留 7 天，平台提供「跨节点并行 grep」（下发检索指令到 2 台 Agent，聚合结果）

代价：失去秒级回溯任意请求的能力。**建议** v0.2 先用轻量方案，日志量确认上来后 v0.3 再上 ClickHouse。

---

## 10. 监控子系统（Prometheus + Grafana + 自研业务视角）

**决策（DECISIONS §10 / §11）**：采集与可视化**不自研**，直接用 Prometheus + Grafana（成熟、省事）；平台只**自研"业务视角"指标与告警汇聚层**，不重复造轮子。

### 10.1 分层

```
[Agent /metrics]  [vSphere Exporter]  [node_exporter]
        │                │                  │
        └────────────────┼──────────────────┘
                         ▼
                  Prometheus（存储/告警规则）
                         │
              ┌──────────┴──────────┐
              ▼                     ▼
        Grafana 看板        Alertmanager ──webhook──▶ 平台告警中心（复用 M6 事件模型）
       （iframe 内嵌，             （与日志安全告警同一张表展示）
        匿名只读 org）
```

### 10.2 采集源

| 源 | 内容 | 暴露方式 |
| --- | --- | --- |
| Agent | stub_status（Active/Reading/Writing/Waiting）+ 系统（CPU/内存/磁盘）+ 进程存活 | `/metrics`，仅控制面可访问 |
| vSphere Exporter | 每台 VM 的 CPU/内存/**万兆链路利用率/LACP 状态** | Prometheus 抓取 |
| 平台自研指标 | 发布成功率、配置漂移节点数、证书剩余天数、DR 合规失败数 | 平台自身 `/metrics` |

### 10.3 自研业务指标（平台侧）

```go
ngxcp_deploy_success_total / ngxcp_deploy_failure_total
ngxcp_config_drift_nodes
ngxcp_cert_days_left{min="7"}     // 证书剩余天数
ngxcp_dr_compliance_fail
```

### 10.4 看板与告警

- **4 张看板**（总览 / Nginx / LVS / VM），经 `provisioning` 自动导入，**禁止手工在 UI 点**（防环境漂移）
- Alertmanager `webhook` → 平台告警中心；与日志安全告警同一中心展示
- 告警 `for` 时长要够（vMotion 期间有几百毫秒中断，避免误报）

### 10.5 时钟同步（虚拟化特有 ★）

- **必须关 VMware Tools 时间同步**（与 chrony 互殴会导致时钟跳变，直接破坏跨节点日志时序与 TraceID 追踪）
- 平台校验两两时钟偏差：**> 1s WARN / > 5s CRITICAL**；容忍窗口 > 3s（vMotion 抖动）
- 详见 DECISIONS §16、AGENTS §9.6

> 完整任务见 `docs/tasks/M7-monitoring.md`。

---

## 11. 构建与升级中心

**痛点（用户明确需求）**：`nginx_upstream_check_module` 要升级 → nginx 必须同步升到支持的版本；手工编译易遗漏 `--with-stream`/`--with-http_v3_module` 等参数，且双机编译不一致会导致一台 reload 即崩。

### 11.1 版本矩阵（模型）

```go
type NginxBuild struct {
    ID, Version, OpenSSL string
    Modules      []ModuleVer   // [{name:"nginx_upstream_check_module", ver:"0.4.x"}]
    ConfigureArgs string       // 完整编译参数，入库归一化
    ChecksumSHA  string
}
// 双机编译一致性：两台 nginx 的 ConfigureArgs 除路径外必须一致
// 兼容表：给定 nginx 版本 → 返回兼容的模块版本（未知版本报错而非瞎猜）
```

### 11.2 可复现构建

- 构建容器**基于目标系统同代基础镜像**（如 rockylinux:9），保证 glibc 兼容
- 编译参数从版本矩阵来，不手写；产物 `tar.gz` + `sha256` + 构建者签名
- 下发前**验签**，防构建链投毒

### 11.3 热升级（Agent 指令 `UpgradeBinary`）

```
下载产物(mTLS) → sha256 校验 → staging
  → 新二进制 nginx -t（目标 conf 完整上下文）
  → USR2 启新 master（旧 worker 继续服务）
  → 探活（80+443 握手）
  → QUIT 旧 master（优雅退出旧 worker）
  → 失败则 QUIT 新 master + 重启旧 master
```

### 11.4 升级编排（zero 5xx）

结合 LVS 权重摘除，**1+1 串行**：先摘 RS2 权重 → 升级 RS2 → 探活 → 加回 → 再动 RS1。摘除期间流量只在另一台，用户侧零 5xx。

### 11.5 前置检查（必须全过）

编译一致性 + DR 合规（§7.2）+ 磁盘空间 + **发布前快照（§4）**；任一不过阻断升级。

> 完整任务见 `docs/tasks/M8-build-upgrade.md`；决策依据 `docs/DECISIONS.md` §12（编译升级与模块管理）、§13（Agent 能力发现）。

## 12. 安全设计

| 层面 | 措施 |
| --- | --- |
| **账号体系** | 本地账号（无 LDAP 需求）；密码 argon2id 存储；用户表预留 `external_id` 字段，将来接 LDAP/OAuth 不改表 |
| **双因素** | TOTP（**建议强制开启** —— 这个面板能改线上配置，被拿下等于全站沦陷） |
| **角色** | 3 个内置：`admin`（全部）/ `operator`（改配置 + 发布，不可改用户与系统设置）/ `viewer`（只读） |
| **登录保护** | 同 IP 连续 5 次失败锁定 15 分钟 |
| **面板暴露面** | **不暴露公网**：仅内网 / Tailscale / WireGuard / SSH 隧道访问 |

---

## 13. 目录结构建议（代码仓库）

```
nginx-cluster-manager/
├── server/                     # 控制面（Go）
│   ├── cmd/ngxcp-server/
│   ├── internal/
│   │   ├── api/                # HTTP handlers
│   │   ├── agent/              # gRPC 服务 + 会话管理
│   │   ├── config/             # 配置文件/版本/模板渲染
│   │   ├── deploy/             # 发布引擎状态机
│   │   ├── cert/               # ACME + 生命周期
│   │   ├── lvs/                # ipvsadm / keepalived 建模
│   │   ├── backup/             # 快照
│   │   ├── scheduler/          # 漂移检测 / 证书扫描 / 心跳超时
│   │   ├── auth/               # 认证 + RBAC
│   │   └── store/              # PG/Redis/Git 访问层
│   └── proto/                  # 与 Agent 共享的 protobuf
├── agent/                      # 数据面 Agent（Go）
│   ├── cmd/ngxcp-agent/
│   └── internal/
│       ├── conn/               # 长连接 + 重连
│       ├── exec/               # 指令执行器（白名单守卫）
│       ├── probe/              # nginx 探测 / 环境自检
│       └── drcheck/            # DR 模式合规检查
├── web/                        # 前端（Vue 3）
│   └── src/views/{dashboard,node,config,deploy,cert,lvs,backup,system}
├── deploy/
│   ├── docker-compose.yaml
│   └── systemd/
└── docs/
    ├── PRD.md
    ├── ARCHITECTURE.md
    └── API.md
```

---

## 14. 核心 API 清单（v1 摘要）

```
# 认证
POST   /api/v1/auth/login
POST   /api/v1/auth/logout
GET    /api/v1/auth/me

# 集群与节点
GET    /api/v1/clusters                    POST   /api/v1/clusters
GET    /api/v1/clusters/:id                PUT    /api/v1/clusters/:id      DELETE /api/v1/clusters/:id
GET    /api/v1/nodes                       POST   /api/v1/nodes/enroll-token
GET    /api/v1/nodes/:id                   PUT    /api/v1/nodes/:id         DELETE /api/v1/nodes/:id
POST   /api/v1/nodes/:id/actions           # test|reload|restart|stop|drcheck
POST   /api/v1/nodes/batch-actions

# 配置
GET    /api/v1/configs?cluster_id=         POST   /api/v1/configs
GET    /api/v1/configs/:id                 PUT    /api/v1/configs/:id
GET    /api/v1/configs/:id/versions        GET    /api/v1/configs/:id/diff?v1=&v2=
POST   /api/v1/configs/:id/validate        # 语法 + 语义校验
POST   /api/v1/configs/render-preview      # 模板渲染预览（按节点）
GET    /api/v1/nodes/:id/config-files      # 回读节点真实配置

# 发布
GET    /api/v1/deploys                     POST   /api/v1/deploys
GET    /api/v1/deploys/:id                 # 含批次与节点结果
POST   /api/v1/deploys/:id/pause           POST   /api/v1/deploys/:id/resume
POST   /api/v1/deploys/:id/abort           POST   /api/v1/deploys/:id/rollback
POST   /api/v1/deploys/:id/batches/:seq/retry

# 证书
GET    /api/v1/certs                       POST   /api/v1/certs/upload
POST   /api/v1/certs/acme/order            POST   /api/v1/certs/:id/renew
POST   /api/v1/certs/:id/deploy            DELETE /api/v1/certs/:id

# LVS
GET    /api/v1/lvs/directors               POST   /api/v1/lvs/directors
GET    /api/v1/lvs/virtual-services        POST   /api/v1/lvs/virtual-services
PUT    /api/v1/lvs/virtual-services/:id    DELETE /api/v1/lvs/virtual-services/:id
POST   /api/v1/lvs/real-servers            PUT    /api/v1/lvs/real-servers/:id
POST   /api/v1/lvs/real-servers/:id/drain  # 权重置 0
POST   /api/v1/lvs/apply                   # 下发到 director
GET    /api/v1/lvs/topology

# 能力基线（nginx -V）
GET    /api/v1/nodes/:id/capability        # 版本/prefix/模块清单
GET    /api/v1/nodes/capability-diff       # 双机编译一致性对比
POST   /api/v1/nodes/:id/capability/refresh

# 日志检索
POST   /api/v1/logs/search                 # 多维筛选（时间/节点/状态码/URI/IP/rid/rt）
GET    /api/v1/logs/trace/:request_id      # TraceID 全链路追踪
POST   /api/v1/logs/aggregate              # Top URI/IP/UA、状态码分布、P50/P95/P99
GET    /api/v1/logs/saved-queries          POST   /api/v1/logs/saved-queries
POST   /api/v1/nodes/grep                  # 轻量模式：跨节点并行 grep

# 安全预警
GET    /api/v1/security/rules              PUT    /api/v1/security/rules/:id
GET    /api/v1/security/events?level=&rule=&handled=
POST   /api/v1/security/events/:id/block   # 手动封禁 → 生成变更单
POST   /api/v1/security/events/:id/ignore  # 忽略（加入白名单）
GET    /api/v1/security/blocklist          # 当前封禁 IP 列表
DELETE /api/v1/security/blocklist/:ip      # 解封（同样走发布流水线）

# 备份
GET    /api/v1/snapshots                   POST   /api/v1/snapshots
POST   /api/v1/snapshots/:id/restore

# 系统
GET    /api/v1/audit-logs                  GET    /api/v1/users  POST /api/v1/users
GET    /api/v1/settings                    PUT    /api/v1/settings
```

---

## 15. 演进路线

| 阶段 | 形态 | 说明 |
| --- | --- | --- |
| 起步（<50 节点） | 单二进制 + SQLite + 本地快照目录 | `./ngxcp-server` 一条命令跑起来，5 分钟可用 |
| 成长（50~300 节点） | + PostgreSQL + Redis | 支持多实例部署，Agent 会话通过 Redis 路由 |
| 规模（300+ 节点） | + 消息队列 + 对象存储 + 控制面集群 | 发布任务异步化，快照走 S3/MinIO |

> 数据库与存储在代码层做接口抽象，从小到大的切换不改业务代码。
