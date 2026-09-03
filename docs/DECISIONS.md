# NGX-CP 架构决策记录（ADR）

> 本文针对你的**真实环境**逐条给出选型结论与理由。
> 环境基线：2 台 Keepalived（主备）+ 2 台 Nginx RS（DR 模式）、Nginx 1.30.0 编译安装、Cloudflare DNS、自用规模、Agent 常驻、无 LDAP 对接需求。
> 文末附「最终技术栈定稿表」。

---

## 决策 0 · 先纠正一个前提：规模决定一切

上一版方案是按「几十到几百节点、多人协作」设计的，**对你的 2+2 环境属于严重过度设计**。先做减法：

| 原设计 | 校准后 | 理由 |
| --- | --- | --- |
| PostgreSQL 14 | **SQLite（WAL 模式）** | 4 个节点，写 QPS 个位数。单文件即全量备份，`cp` 走天下。用 sqlc + 仓储层抽象，将来换 PG 只改 driver |
| Redis（队列/锁/会话路由） | **删除**，进程内 goroutine 调度 + SQLite 事务锁 | 4 个 Agent 长连接直接放内存 map；变更锁用 `UPDATE ... WHERE` 乐观锁即可 |
| Prometheus + Grafana | **延后到 v0.3**，先用 Agent 心跳 + `stub_status` 存 SQLite 时序表 | 2 节点的指标一张仪表盘能看完，不值得引入两个组件 |
| 控制面主备 / 容器编排 | **单实例 systemd 进程** | 控制面不参与流量转发，挂了业务无感，只是暂时不能发版 |
| 灰度 5%→20%→50%→100% | **1+1 串行**（见决策 5.3） | 2 节点做百分比没有数学意义 |
| 500 节点压测验收 | **删除** | 无意义 |
| Git 仓库存配置 | **改为 SQLite 内容寻址**（见决策 2） | 备份单个文件 vs 备份「DB + Git 目录」两套 |

**控制面资源建议**：2C4G / 60GB SSD（含 7 天全量日志）。可以跑在一台独立的轻量云主机上，**不建议跑在 4 台业务机任意一台**（控制面出故障时你正在排障，别让它们互相干扰）。

---

## 决策 1 · 配置同步：scp / rsync / Agent 内建传输

### 1.1 scp 已经不该用了

不是"好不好用"的问题，是它**正在被淘汰**：

- OpenSSH **9.0（2022-04）起，`scp` 默认改用 SFTP 协议**实现，传统 SCP 协议进入 deprecated 状态
- 传统 SCP 协议的历史漏洞：`CVE-2019-6111`（服务端可覆写任意文件名）、`CVE-2019-6109`（路径遍历）、`CVE-2020-15778`（命令注入）—— 根因是**文件名与通配符由服务端解释**，这个设计本身就不安全
- 即便用新版 scp：无增量、无断点续传、无 dry-run、无删除同步、传输中断留下残缺文件

**结论：scp 出局。** 若确实需要走 SSH 通道，用 `rsync -e ssh` 或 `sftp`，不要再用 scp 协议。

### 1.2 rsync 比 scp 强在哪（客观评价）

| 能力 | scp | rsync |
| --- | --- | --- |
| 增量传输（delta 算法，只传差异块） | ✗ | ✓ |
| 断点续传 | ✗ | ✓（`-P`） |
| 删除目标端多余文件 | ✗ | ✓（`--delete`） |
| 预演（dry-run） | ✗ | ✓（`--dry-run`） |
| 按内容而非 mtime 判断是否需要传 | ✗ | ✓（`--checksum`） |
| 带宽限速 | ✗ | ✓（`--bwlimit`） |
| 覆盖前备份旧文件 | ✗ | ✓（`--backup --suffix`） |
| 保留权限/属主/时间戳/软链 | 部分 | ✓（`-a`） |
| 目录树整体同步效率 | 差（逐文件全量） | 优（单进程全量扫描后差量传） |

rsync 确实是**传输层**的正确答案，你的判断是对的。

### 1.3 你说的"不是操作系统预装"—— 这个缺点被高估了

- 主流发行版 rsync 都在**基础源**里：`yum install -y rsync` / `apt install -y rsync` 一行解决，CentOS/RHEL 的 minimal ISO 里就有
- 真正没有的场景：Alpine 基础镜像、scratch/distroless 容器镜像、部分嵌入式系统
- **你是编译安装 Nginx 的物理机/VM，rsync 要么已有，要么一条命令装上**

所以"非预装"并非决定性缺陷。

### 1.4 但 rsync 解决不了配置管理的五个真问题

这是关键 —— **rsync 缺的不是传输能力，是编排语义**：

| 问题 | rsync 的表现 | 后果 |
| --- | --- | --- |
| **① 原子性** | 传到一半网络断了 | 目标节点是"半新半旧"状态，`nginx -t` 必失败，但目录已被污染 |
| **② 校验门禁** | rsync 完直接 reload | 配置有语义错误时，**两台同时炸**，服务全断 |
| **③ 版本历史** | rsync 只同步最终状态 | 历史在源端；节点上"上一个版本是什么"无从得知，回滚要另建机制 |
| **④ 漂移感知** | 节点上手工改过，下次被静默覆盖 | 你永远不会知道有人在 02 机器上救急改过东西 |
| **⑤ 审计** | rsync 日志只有一行 syslog | "谁、何时、从哪、推了什么给谁"没有可查询的记录 |

另外 rsync 是**推送模型**（控制面主动连节点），跨机房/NAT/无公网 IP 的节点要开 SSH 端口、管密钥，这是运维负担。

### 1.5 最终结论：分层处理

```
┌─ 编排层（控制面负责）──────────────────────────┐
│  版本 · 校验 · 灰度 · 探活 · 回滚 · 审计 · 漂移检测 │
└──────────────────┬──────────────────────────┘
┌─ 传输层（Agent 内建，默认）────────────────────┐
│  mTLS 长连接 · 分块传输 · SHA256 校验 · 原子落盘  │
└──────────────────────────────────────────────┘
┌─ 传输层备选（rsync，仅用于大体积/目录树）────────┐
│  静态资源 · GeoIP 库 · 离线包 · 首次全量同步      │
└──────────────────────────────────────────────┘
```

**① 默认走 Agent 内建传输**（Go 实现，不依赖节点上任何外部命令）。既然 Agent 已经常驻，再依赖 scp/rsync 纯属多余。

**② 保留 rsync 作为可选通道**，用于 Agent 尚未覆盖的场景：同步 `/usr/share/nginx/html` 静态资源、几十 MB 的 GeoIP 库、首次纳管时的全量拉取。使用时纳入同一条发布记录（先 `--dry-run` 记录将变更的文件列表 → 真实执行 → 后置校验）。

**③ 弃用 scp。**

### 1.6 Agent 内建传输的核心：原子落盘序列

这是 rsync 给不了的东西，也是"零污染"的技术根据：

```bash
# 1. 传输到隔离暂存区（与 /etc/nginx 同分区，保证 rename 原子）
/var/lib/ngxcp/staging/<task_id>/nginx.conf
# 2. 校验 SHA256，与下发时声明的摘要比对，不符即中止
# 3. 在暂存区执行语法校验（用 -p 指定 prefix，保证相对路径语义一致）
nginx -t -p /etc/nginx -c /var/lib/ngxcp/staging/<task_id>/nginx.conf
#    ↓ 失败 → 删除 staging，线上配置一个字节都没动，任务标记失败
# 4. 通过则先做现状快照
tar czf /var/lib/ngxcp/snapshots/<node>/<ts>.tar.gz -C /etc/nginx .
# 5. 原子切换（同一分区内 rename 是原子操作，不存在"半个文件"）
mv /var/lib/ngxcp/staging/<task_id>/* /etc/nginx/
# 6. 平滑加载 + 探活
nginx -s reload && curl -sf -o /dev/null -w '%{http_code}' http://127.0.0.1/health
#    ↓ 探活失败 → 从步骤 4 的快照恢复 → reload → 任务标记回滚
```

要点：**校验发生在切换之前**，所以任何错误都不可能污染线上。rsync 做不到这一点，因为它是"边传边覆盖"。

---

## 决策 2 · 配置存储：Git 还是数据库

### 2.1 先澄清一个误解：节点上从来不需要 Git

你担心"不是所有地方都有 git"—— 但**配置仓库只存在于控制面那一台机器上**，节点上只需要落盘后的普通配置文件。Agent 不读 Git、不装 Git、不知道 Git 存在。

所以这个约束实际上被架构消解了。真正要比的是：**控制面上用 Git 存版本，还是用数据库存版本**。

### 2.2 三选一对比

| 方案 | 做法 | 优势 | 劣势 |
| --- | --- | --- | --- |
| **A. 系统 git 二进制 + 裸仓** | 控制面 `git init --bare`，go-git 或 exec 调用 | diff/blame/branch/merge 全免费；可直接用命令行排查；可 push 到远端做异地备份 | 多一个外部依赖；多一套东西要备份；**关系查询答不了**；私钥进 git 历史就永久留着 |
| **B. go-git 内嵌库** | 纯 Go 实现，编译进二进制 | 无外部命令依赖；具备 A 的全部能力 | 二进制 +10MB；**并发写要自己串行化**；仍需备份仓库目录；私钥问题同 A |
| **C. SQLite + 内容寻址 blob** | 内容存 blob 表（SHA256 去重），版本表记录血缘 | **单文件备份**；事务并发安全；关系查询随心所欲；**私钥不进版本历史**；查询时算 diff | 分支语义缺失（v1 用不上）；diff 需自研（约 1~2 人天） |

### 2.3 结论：选 C，保留 B 作为可选导出

**推荐方案 C（SQLite + 内容寻址 blob），理由按权重排序：**

1. **备份是单个文件** —— 自用场景的决定性因素。`sqlite3 ngxcp.db ".backup /backup/ngxcp-$(date +%F).db"` 一条 cron 就完成全量备份（配置 + 任务 + 节点 + 告警 + 用户）。方案 A/B 要同时备份 DB 和 Git 目录，还得保证两者一致。
2. **关系查询是刚需** —— "这个配置文件被哪几个节点引用""上次发布后哪些节点还没追上版本""这张证书关联哪些 server_name"，这些 Git 一律答不了，最终还是得落到 DB。既然 DB 必须有，就别维护第二套存储。
3. **你的配置总量很小** —— 2 个节点，配置文件撑死几百 KB。全量历史永久保存毫无压力（对比：10 年 × 每天 5 次变更 × 50KB ≈ 900MB，可接受）。
4. **私钥不进版本历史** —— 证书私钥加密后存独立表，即使误操作也不会像 Git 那样在历史里永久留痕。
5. **diff 不算事** —— 配置文本 < 100KB，用 `sergi/go-diff` 或 `hexops/gotextdiff` 在读取时实时计算，毫秒级，UI 体验不输 GitHub。

**方案 B 保留为可选增强**：系统设置里一个开关「同步配置历史到 Git 仓库」，开启后每次提交自动 commit 到本地裸仓，可 push 到私有远端做异地容灾。默认关闭，需要时一键打开 —— 这样既不增加默认运维负担，又保留了 Git 的所有好处。

### 2.4 数据模型

```sql
-- 内容寻址存储：相同内容只存一份，天然去重
CREATE TABLE config_blob (
    sha256     TEXT PRIMARY KEY,
    content    BLOB NOT NULL,
    size       INTEGER NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 逻辑文件（按集群隔离）
CREATE TABLE config_file (
    id            INTEGER PRIMARY KEY,
    cluster_id    INTEGER NOT NULL,
    path          TEXT    NOT NULL,        -- /etc/nginx/conf.d/api-gateway.conf
    kind          TEXT    NOT NULL,        -- nginx | keepalived | snippet | stream
    current_sha   TEXT    NOT NULL REFERENCES config_blob(sha256),
    updated_at    DATETIME NOT NULL,
    UNIQUE(cluster_id, path)
);

-- 版本血缘：parent 串成链，可回溯任意历史
CREATE TABLE config_revision (
    id          INTEGER PRIMARY KEY,
    file_id     INTEGER NOT NULL REFERENCES config_file(id),
    version     INTEGER NOT NULL,          -- 文件内自增
    blob_sha    TEXT    NOT NULL REFERENCES config_blob(sha256),
    parent_id   INTEGER REFERENCES config_revision(id),
    author      TEXT    NOT NULL,
    message     TEXT,
    source      TEXT    NOT NULL,          -- manual | template | api | cert-renew | rollback
    created_at  DATETIME NOT NULL,
    UNIQUE(file_id, version)
);

-- 文件与节点的绑定关系（回答"这个文件推给谁"）
CREATE TABLE config_binding (
    file_id  INTEGER NOT NULL REFERENCES config_file(id),
    group_id INTEGER NOT NULL,             -- 集群分组
    enabled  INTEGER NOT NULL DEFAULT 1
);
```

**回滚的语义**：不是"把历史内容覆盖回节点"，而是「把某个历史 revision 的 blob 设为 current，然后**重新走一次完整的发布流水线**（校验 → 灰度 → 探活）」。这样回滚本身也是一次受控变更，有记录、可再次回滚。

**diff 的语义**：`config_revision` 表拿到两个版本的 `blob_sha` → 取出内容 → 行级 diff → 渲染。跨文件、跨版本、跨环境都是同一套逻辑。

---

## 决策 3 · 日志统一管理与攻击预警

这是你问的最有价值的问题。Nginx 集群化后日志排查难，本质是**三个断裂**：请求散落在不同节点（不知道去哪台查）、日志格式不统一（查到了也对不齐）、没有关联 ID（同一个会话的多段日志串不起来）。

按五层来解决。

### 3.1 第 0 层：先让日志可关联 —— TraceID（零成本，收益最大）

Nginx 从 1.11.0 起内置 `$request_id`（32 位十六进制，每请求唯一）。三件事：

```nginx
# ① 写进日志
log_format ngxcp_json escape=json '{'
  '"request_id":"$request_id",'
  ...
'}';

# ② 回写给客户端（用户报错时，让他从响应头里复制给你）
add_header X-Request-Id $request_id always;

# ③ 透传给后端（形成全链路）
proxy_set_header X-Request-Id $request_id;
```

**效果**：用户报障时只要拿到响应头里的 `X-Request-Id`，你就能在平台上一次检索，直接定位到"这条请求打在 nginx-02、转发到 10.0.0.21:8080、耗时 1.2s"—— 不用再去两台机器上 `grep`。

**DR 模式的额外好处**：DR 模式下 RS 收到的包，**目标 IP 是 VIP、源 IP 是真实客户端 IP**（这正是 DR 相对 NAT 的核心优势），所以 `$remote_addr` 天然就是真实 IP，不需要 realip 模块处理 VIP 干扰。但如果前面还挂了 CDN/云 WAF，仍需：

```nginx
set_real_ip_from 173.245.48.0/20;   # Cloudflare IP 段（需定期更新）
real_ip_header   CF-Connecting-IP;  # 或 X-Forwarded-For
```

> **平台要做的**：Cloudflare 的 IP 段会变，平台提供「更新 CDN IP 段」按钮，自动拉 `https://api.cloudflare.com/client/v4/ips` 生成 `set_real_ip_from` 片段并纳入版本管理。

### 3.2 第 1 层：统一日志格式

给出推荐格式（**平台在纳管节点时可一键生成这段配置**）：

```nginx
log_format ngxcp escape=json '{'
    '"ts":"$time_iso8601",'
    '"rid":"$request_id",'
    '"node":"$hostname",'
    '"host":"$host",'
    '"ip":"$remote_addr",'
    '"xff":"$http_x_forwarded_for",'
    '"method":"$request_method",'
    '"uri":"$request_uri",'
    '"proto":"$server_protocol",'
    '"status":$status,'
    '"bytes":$body_bytes_sent,'
    '"rt":$request_time,'
    '"urt":$upstream_response_time,'
    '"uaddr":"$upstream_addr",'
    '"ustatus":"$upstream_status",'
    '"uct":$upstream_connect_time,'
    '"referer":"$http_referer",'
    '"ua":"$http_user_agent",'
    '"ssl_p":"$ssl_protocol",'
    '"ssl_c":"$ssl_cipher"'
'}';

access_log /var/log/nginx/access.log ngxcp;
```

**三个字段是排查命门**：

- `upstream_addr` —— 一眼看出请求落在哪个后端（多个值逗号分隔 = 发生了重试）
- `upstream_response_time` —— 慢在后端还是慢在 Nginx（对比 `request_time`）
- `upstream_status` —— 出现 `502` 时，后端到底返回了什么

错误日志同样要结构化（Nginx 1.11.8+ 支持 `error_log ... json` 需要编译时带 `http_v2`？不需要，1.11.8+ 原生支持），至少保证 `error.log` 被采集，因为 **SSL 握手失败、upstream 连接拒绝、limit_req 触发**这些都只出现在 error.log。

### 3.3 第 2 层：采集 —— 直接做进 Agent，别再引一个组件

| 方案 | 体积/内存 | 优点 | 缺点 |
| --- | --- | --- | --- |
| **Agent 内置 tail 模块** | 0（复用进程） | 少一个组件要运维、要升级、要监控；与配置下发共用 mTLS 通道 | 采集逻辑出 bug 有风险（靠 goroutine 隔离 + recover 兜底） |
| Vector | ~50MB | 功能强（多行合并、采样、路由、丰富 sink） | 多一个进程、多一份配置、多一个升级面 |
| Fluent Bit | ~20MB | 轻量，CNCF 毕业项目 | 配置语法自成一套，学习成本 |
| Filebeat | ~100MB+ | Elastic 生态成熟 | 最重，2 节点没必要 |

**结论：Agent 内置采集模块（默认开），Vector 作为流量上来后的可选增强。**

实现要点：
- 独立 goroutine + `defer recover()`，**采集失败绝不影响配置下发主流程**
- offset 持久化到本地（重启不丢不重）
- 本地磁盘队列缓冲（控制面挂了先攒着，恢复后补传，默认攒 500MB 或 24 小时）
- 批量压缩上传（默认 1000 条 或 5 秒 触发一次）
- **采样降载**：可配「状态码 ≥ 500 全采；2xx/3xx 按 10% 采样」，日志量大时救磁盘

### 3.4 第 3 层：存储 —— ClickHouse（限制内存）为主，VictoriaLogs 备选

先估量：2 节点自用，假设日 PV 100 万，单条 JSON ~400 字节 → **原始 ~400MB/天**。

| 方案 | 压缩后/天 | 内存 | 查询 | 结论 |
| --- | --- | --- | --- | --- |
| **ClickHouse** | ~40–80MB | 可限制 `max_memory_usage=2GB` | **标准 SQL，运维零学习成本** | ✅ 推荐 |
| VictoriaLogs | ~50–100MB | ~500MB | LogsQL（类管道语法） | 🟡 控制面只有 2G 内存时选它 |
| Grafana Loki | ~60–120MB | ~1GB | LogQL | 🟡 但高基数标签（rid/uri）会炸，需刻意规避 |
| OpenSearch | ~200MB+ | 4GB 起 | DSL | ❌ 杀鸡用牛刀 |
| SQLite + FTS5 | 不压缩 | 极低 | SQL | ❌ 超过千万行检索明显变慢 |

**推荐 ClickHouse 单实例**，决定性理由：**攻击检测规则本质上就是 SQL 聚合查询**。你是运维，SQL 的心智负担远低于 LogQL/DSL/LogsQL —— 写规则、调规则、临时排查，全用同一套 SQL。

表结构与关键参数：

```sql
CREATE TABLE nginx_access (
    ts        DateTime64(3),
    node      LowCardinality(String),
    rid       String,
    host      LowCardinality(String),
    ip        IPv4,                       -- 用 IPv4 类型，比 String 省一半空间
    method    LowCardinality(String),
    uri       String,
    status    UInt16,
    bytes     UInt32,
    rt        Float32,
    urt       Float32,
    uaddr     String,
    ustatus   String,
    ua        String,
    referer   String
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (ts, node, status)
TTL ts + INTERVAL 7 DAY                   -- 全量日志保留 7 天，可配
SETTINGS index_granularity = 8192;
```

```sql
-- 告警事件永久保留（体积小，且是安全审计依据）
CREATE TABLE security_event (
    ts        DateTime64(3),
    rule      LowCardinality(String),
    level     LowCardinality(String),     -- info | warn | critical
    src_ip    IPv4,
    node      LowCardinality(String),
    hits      UInt32,
    window    UInt32,
    evidence  String,                     -- 抽样样本 JSON
    action    LowCardinality(String),     -- none | pending-review | blocked
    change_id Nullable(Int64)             -- 关联的封禁变更单
)
ENGINE = MergeTree PARTITION BY toYYYYMM(ts) ORDER BY (ts, rule);
```

### 3.5 第 4 层：攻击预警 —— 规则引擎

滑动窗口统计（1 分钟 / 5 分钟 / 1 小时三档），规则表：

| 规则 | 判据 | 级别 | 默认动作 | 误报风险 |
| --- | --- | --- | --- | --- |
| **注入特征** | URI/UA/Referer 正则命中 SQLi / XSS / 命令注入 / 路径穿越特征 | CRITICAL | 自动生成封禁单 + 自动执行 | 低 |
| **扫描器指纹** | UA 匹配 `sqlmap\|nmap\|nikto\|masscan\|nessus\|acunetix\|dirbuster` | CRITICAL | 自动生成封禁单 + 自动执行 | 极低 |
| **目录爆破** | 单 IP 60s 内 `404 ≥ 50` 且 **去重 URI ≥ 30** | CRITICAL | 生成封禁单，待审批 | 中（爬虫会误报） |
| **CC 洪水** | 单 IP 60s 请求数 > `max(600, 7天基线均值 × 8)` | CRITICAL | 生成封禁单，待审批 | 中（移动网络 NAT 出口） |
| **敏感路径探测** | 命中 `/.env` `/.git` `/wp-login` `/phpmyadmin` `/actuator` `/adminer` | WARN | 仅记录 | 高（很多是误配的爬虫） |
| **慢速攻击** | 单连接 `rt > 30s` 并发数异常（Slowloris 特征） | WARN | 仅记录 | 中 |
| **5xx 突增** | 全集群 5xx 率 5 分钟 > 5% 且环比 +300% | CRITICAL | 告警（**这是故障不是攻击**，要区分） | 低 |
| **4xx 突增** | 403/404 率环比 +200% | INFO | 仅记录（可能是刚发的配置写错了） | 中 |
| **异常地理/ASN** | GeoIP 命中非业务地区且高频 | INFO | 仅记录 | 高 |
| **SSL 握手失败潮** | error.log 中 handshake failure 激增（扫描器/老旧客户端探测） | WARN | 仅记录 | 中 |

**关键设计：封禁不是"平台直接改线上配置"，而是生成一张封禁变更单，复用已有的发布流水线。**

```
检测到攻击
  → 生成 deny 片段（追加到 /etc/nginx/conf.d/zz-blocklist.conf）
  → 创建一条配置变更单（source = 'security'）
  → 走完整的 校验 → 灰度 → 探活 → 回滚 链路
  → 全程审计留痕
```

这么做的好处：

1. **封禁也可回滚** —— 自动封禁把你的 IP 封了，一键撤回，而不是去机器上删行
2. **封禁也可灰度** —— 先在 1 台生效观察，避免规则写错把正常流量全拒
3. **封禁有审批** —— 高误报规则默认走人工确认，不自动执行
4. **零新增机制** —— 复用配置中心 + 发布引擎 + 审计日志，不引入第二条写线上的路径（这点很重要：任何绕过发布流水线的"便捷通道"，最终都会变成事故来源）

**分级处置策略（可配）**：

| 模式 | 触发条件 | 行为 |
| --- | --- | --- |
| 自动 | CRITICAL + 高置信规则（注入特征、扫描器指纹） | 生成变更单 → 自动执行 → 通知 |
| 半自动 | CRITICAL + 中置信（CC、目录爆破） | 生成变更单 → 等审批 → 通知 |
| 仅告警 | WARN / INFO | 只记录，可在告警页手动「一键封禁」 |

### 3.6 第 5 层：排查交互（UI 设计要点）

日志检索页必须有的能力：

- **时间快选**：5m / 1h / 24h / 7d / 自定义
- **筛选**：节点 · 集群 · 状态码 · 方法 · URI（精确/前缀/正则）· 客户端 IP · `request_id` · `rt > N` · upstream 地址
- **TraceID 追踪**：输入 ID → 展示这条请求的完整链路（哪个 nginx 节点 → 打到哪个 upstream → 各段耗时）
- **聚合视图**：Top URI / Top IP / Top UA / 状态码分布 / P50·P95·P99 响应时间曲线
- **保存的查询**：常用排查场景（"昨天的 502""最近的慢请求"）存成快捷方式
- **原文展开**：表格点开看完整 JSON 字段，不用去机器上翻

> 补充：你编译了 `nginx_upstream_check_module`，它提供 `/status` 页面。平台应当解析它 → 后端健康度面板。当某 RS 被标记为 down 时触发告警 —— 这类是**故障信号**，与攻击检测互补，不要混为一谈。

### 3.7 如果嫌重：轻量降级方案

不想上 ClickHouse 的话，可以走**两级存储**：

- **Agent 本地实时检测** —— 在节点上滑动窗口统计，只把命中的告警事件上报控制面（存 SQLite）
- **全量日志不落中心** —— 各节点本地保留 7 天，平台提供「跨节点并行 grep」按钮（下发检索指令到 2 台 Agent，聚合结果）

代价：失去秒级回溯任意请求的能力，排查要等 Agent 回传。
**建议**：先用轻量方案跑起来（v0.2），确认日志量真的上来了再上 ClickHouse（v0.3）。

---

## 决策 4 · LVS + Keepalived DR：合规基线与无损发布

你的架构是 **2 台 Director（Keepalived 主备）+ 2 台 RS（Nginx），DR 模式**。这是最经典也最容易踩坑的组合。

> ⚠️ **需确认**：你说"2 台 keepalived + 2 个 nginx 节点"，我按 **4 台独立机器**理解（2 Director + 2 RS）。如果 Keepalived 是跑在 Nginx 同机上（2 台机器兼做 Director 和 RS），请告我 —— 后者是另一种拓扑，VIP 与 RS 同机时 ARP 处理要额外小心。

### 4.1 DR 模式的六个硬约束（纳管时必须自动检查）

| # | 约束 | 正确做法 | 违反后果 |
| --- | --- | --- | --- |
| 1 | **VIP 绑在 RS 的 lo 上，掩码 /32** | `ip addr add 10.0.0.100/32 dev lo label lo:0` | 绑 /24 会导致路由表歧义，部分流量绕不过来 |
| 2 | **ARP 抑制（最关键）** | `net.ipv4.conf.all.arp_ignore=1`<br>`net.ipv4.conf.all.arp_announce=2`<br>`net.ipv4.conf.lo.arp_ignore=1`<br>`net.ipv4.conf.lo.arp_announce=2` | RS 会响应 VIP 的 ARP 请求，与 Director 抢答 → **流量时通时断，最难排查** |
| 3 | **反向路径过滤关闭** | `net.ipv4.conf.all.rp_filter=0`<br>`net.ipv4.conf.{default,eth0}.rp_filter=0` | 严格模式下回包源 IP 是 VIP 但出口是 eth0 → 内核直接丢包 |
| 4 | **VIP 绝不能配在物理网卡** | 只能 lo | 与约束 2 同理，ARP 混乱 |
| 5 | **不支持端口映射** | VIP:80 → RS:80，端口必须一致 | DR 只改 MAC 不改 IP 头，端口改不了 |
| 6 | **Director 与 RS 必须同二层** | 同 VLAN / 同交换机 | 跨三层 DR 不通（NAT/TUN 才行） |

**平台实现**：Agent 每 5 分钟巡检一次，把这 6 项做成「DR 合规基线」。任一项漂移 → 节点标红 → **阻断所有 LVS 相关发布**（但不断业务，只是不让你发）。

### 4.2 Keepalived 主备的七个坑

| # | 问题 | 正确处理 |
| --- | --- | --- |
| 1 | **`virtual_router_id` 冲突** | 同一二层域内必须唯一。多组 VIP 用不同 ID（0–255） |
| 2 | **云厂商禁组播** | 阿里云/腾讯云/华为云的 VPC **不支持 VRRP 组播**，必须配单播：<br>`unicast_src_ip 10.0.0.11`<br>`unicast_peer { 10.0.0.12 }` |
| 3 | **抢占抖动** | 生产建议双 `state BACKUP` + `nopreempt`，主恢复后不抢回，避免切换抖动。<br>**自用场景可配**：主 `MASTER` + ` preempt`，主恢复了就抢回来。做成平台可选项 |
| 4 | **没有健康检查就切换不了** | `vrrp_script` 检测本机 nginx 进程/端口，失败则 `weight -30` 触发降权 |
| 5 | **脑裂（双主同时持 VIP）** | 配 `garp_master_delay 5` + 平台侧监控「VIP 归属」告警（两节点同时上报持 VIP = 脑裂） |
| 6 | **认证机制** | Keepalived 2.x **已移除 AH 认证**，只剩 `auth_type PASS`（明文）。安全靠网络隔离，别指望它 |
| 7 | **切换无感知** | `notify_master` / `notify_backup` / `notify_fault` 脚本 → 上报平台 → 进审计日志 + 推通知 |

### 4.3 平台为这套架构做的四件事

**① keepalived.conf 可视化编辑 + 双机一致性校验**

两台 Director 的配置应当**只有 3 处不同**：`state`、`priority`、`unicast_src_ip`。平台按模板渲染两份，并自动 diff 高亮 —— 如果除了这 3 项之外还有差异，说明配置漂移了，标红提示。

**② RS 合规巡检（见 4.1）**

**③ VIP 漂移事件接入审计日志**

```bash
# keepalived.conf 片段
notify_master "/usr/local/bin/ngxcp-notify master"
notify_backup "/usr/local/bin/ngxcp-notify backup"
notify_fault  "/usr/local/bin/ngxcp-notify fault"
```
脚本 `curl` 上报控制面 → 生成审计事件 → 命中「VIP 切换」规则则推送通知。**主备切换是重大事件，必须可追溯。**

**④ 拓扑图 + 无损摘除（核心能力）**

2 节点场景下，真正的"灰度发布"不是 Nginx 层的百分比，而是 **LVS 层的权重摘除**：

```
发布 nginx-02（RS2）的完整序列：
  1. ipvsadm -e -t VIP:80 -r RS2:80 -w 0      # 权重设 0，新连接不再调度过来
  2. 等待活跃连接排空（轮询 Established 数归零，或最多等 60s）
  3. 下发新配置 + nginx -t + 原子切换 + reload
  4. 本地探活（curl 127.0.0.1/health）+ 从 Director 侧探活 RS2
  5. ipvsadm -e -t VIP:80 -r RS2:80 -w 1      # 加回权重
  6. 观测 60s（5xx 率 / 响应时间 / check 模块健康状态）
  7. 正常 → 对 RS1 重复 1–6；异常 → 立即回滚 RS2 并中止
```

**这才是 LVS+DR 场景下真正的无损发布** —— 用户在整个过程中感知不到任何 5xx。纯 Nginx 层的灰度做不到这一点（reload 瞬间可能有连接被 reset）。

### 4.4 与 nginx_upstream_check_module 的分工

注意区分两层的健康检查，别搞混：

| 层 | 检查者 | 检查对象 | 失效后果 |
| --- | --- | --- | --- |
| **LVS 层** | `keepalived` 的 `TCP_CHECK`/`HTTP_GET` | RS（Nginx 节点）是否存活 | 权重设 0，LVS 不再调度 |
| **Nginx 层** | `nginx_upstream_check_module` 的 `check` 指令 | 后端应用（upstream server）是否存活 | 从 upstream 摘除，不再转发 |

平台要把这两层都可视化，且**告警要区分**：「RS 全部 down」是致命的（业务中断），「某 upstream server down」是降级（还有别的后端）。

---

## 决策 5 · 证书：ACME + Cloudflare，同时支持手动上传

### 5.1 两种来源都要支持（你明确提了）

| 来源 | 适用场景 | 实现 |
| --- | --- | --- |
| **ACME 自动签发** | 普通域名、通配符域名 | Cloudflare DNS-01（你当前 DNS 在 CF） |
| **手动上传** | 商业证书、OV/EV、客户提供的证书、内部 CA | 上传 fullchain + key，平台校验后入库 |

### 5.2 Cloudflare DNS-01 配置

**API Token 权限最小化**：`Zone.Zone:Read` + `Zone.DNS:Edit`，**限定到具体 zone**，不要给全账号权限。

```
Token 存储：AES-256-GCM 加密后存 SQLite
主密钥：环境变量 NGXCP_MASTER_KEY 或 /etc/ngxcp/master.key（权限 0600）
```

**DNS-01 是通配符证书的必要条件**（HTTP-01 签不了 `*.example.com`），且你在 Cloudflare 后面，HTTP-01 还可能被 CDN 拦截 —— 直接上 DNS-01，别纠结。

### 5.3 DNS Provider 抽象层

你把 DNS 从阿里云迁到了 Cloudflare，保不准以后还有别的。设计成接口：

```go
type DNSProvider interface {
    Name() string
    SetRecord(ctx context.Context, zone, name, value string) error   // _acme-challenge TXT
    DeleteRecord(ctx context.Context, zone, name string) error
    Validate() error                                                  // 测试 API 凭据
}
```

**v1 实现 Cloudflare**，预留 `AliyunDNS`（你之前在用，迁移期可能还有域名留在那边）、`DNSPod`、`Route53` 的位置。接口很小，加一个 provider 约 100 行。

### 5.4 手动上传的校验（这块要做扎实）

上传时平台自动做 6 项检查，任何一项不过就拒绝：

| 检查 | 说明 |
| --- | --- |
| **私钥与证书匹配** | 比对证书公钥模数与私钥模数，不匹配直接拒绝（最常见的低级错误） |
| **链完整性** | 是否缺 intermediate。缺链会导致部分客户端（尤其 Android/Java）报不受信任 |
| **链顺序** | 必须是 leaf → intermediate，**不能包含 root**（含 root 会触发部分客户端的额外下载，拖慢握手） |
| **域名覆盖** | SAN 列表是否覆盖所有引用它的 `server_name`。漏一个就是上线后 502 |
| **有效期** | 已过期 / 剩余 < 7 天 → 标红 |
| **签名算法** | SHA-1 / MD5 签名 → 直接拒绝（现代浏览器已不信任） |

### 5.5 分发与安全

```
Agent 拉取（mTLS）→ 内存解密 → 写 /etc/nginx/ssl/<domain>.{crt,key}
  → chmod 0600 key / 0644 crt
  → chown root:root（nginx master 以 root 启动，可读）
  → 原子 rename 切换
  → nginx -t → reload → 探活
```

**私钥安全红线**：
- 私钥**永不下发给浏览器**（API 只返回元数据：域名、签发者、到期时间、指纹、算法）
- 下载私钥需二次确认 + 记审计日志
- 私钥不进配置版本历史（存在独立的 `certificate` 表，与 `config_blob` 隔离）

### 5.6 自动续期

- 到期前 **30 天**开始，每日 03:00 检查
- 续期成功 → 自动创建一条 `source='cert-renew'` 的配置变更单 → **走完整发布流水线**
- 续期失败（如 DNS API 挂了）→ 告警，且**连续 3 天失败升级为 CRITICAL**
- 有个坑：Let's Encrypt 有速率限制（每周每域名 50 张），平台要做本地限流，别在调试时把自己锁了

### 5.7 Nginx 1.30 的证书相关注意

```nginx
# ① ssl_certificate 虽支持变量，但每次握手都读文件，生产用静态路径 + reload
ssl_certificate     /etc/nginx/ssl/example.com.crt;
ssl_certificate_key /etc/nginx/ssl/example.com.key;

# ② 你编译了 --with-http_v3_module，需要 quic 监听 + Alt-Svc 头
listen 443 quic reuseport;
listen 443 ssl;
http2 on;
add_header Alt-Svc 'h3=":443"; ma=86400' always;

# ③ 会话复用（性能）
ssl_session_cache shared:SSL:10m;
ssl_session_timeout 1d;
ssl_session_tickets off;      # 关闭 ticket 更安全，但影响性能，按需选择
```

---

## 决策 6 · Nginx 1.30.0 编译参数 → 能力基线

从你的 configure 参数提取出的关键事实：

| 参数 | 含义 | 对平台的影响 |
| --- | --- | --- |
| `--prefix=/etc/nginx`<br>`--conf-path=/etc/nginx/nginx.conf` | 配置根目录就是 `/etc/nginx` | 平台纳管时**必须读 `nginx -V` 解析 prefix**，不能硬编码 `/usr/local/nginx` |
| `--with-http_v3_module` | HTTP/3 / QUIC | 配置校验需识别 `listen ... quic`、`http3` 指令 |
| `--with-stream`<br>`--with-stream_ssl_module`<br>`--with-stream_ssl_preread_module` | **四层代理 + SNI 预读** | 平台**必须支持 stream 块配置**（大量工具只管 http 块）。`ssl_preread_server_name` 可做基于域名的四层分流 |
| `--with-http_realip_module` | 真实 IP 提取 | 有 CDN 时必需，见决策 3.1 的 `set_real_ip_from` |
| `--with-http_stub_status_module` | 基础状态 | 平台采集 Active/Reading/Writing/Waiting 连接数 |
| `--with-http_gzip_static_module` | 预压缩 `.gz` | 校验时检查 `gzip_static` 用法 |
| `--add-module=../nginx_upstream_check_module` | **主动健康检查** | 识别 `check` 指令；解析 `/status` 页面做后端健康面板 |
| `--with-threads --with-file-aio` | 异步 IO | 信息记录，无特殊处理 |

### 6.1 能力基线机制（重要）

平台纳管节点时执行 `nginx -V` 并解析，存为节点画像：

```json
{
  "version": "1.30.0",
  "prefix": "/etc/nginx",
  "conf_path": "/etc/nginx/nginx.conf",
  "modules": ["http_ssl", "http_v2", "http_v3", "realip", "stub_status",
              "gzip_static", "stream", "stream_ssl", "stream_ssl_preread",
              "nginx_upstream_check_module"],
  "raw_args": "--prefix=/etc/nginx --sbin-path=/usr/sbin/nginx ..."
}
```

然后做三件事：

**① 配置校验时按能力检查**
配置里用了 `check interval=3000` 但目标节点没编译这个模块 → **校验阶段就失败**，而不是等下发后 `nginx -t` 报错。

**② 双机编译一致性检测**（2 节点场景价值极高）
nginx-01 与 nginx-02 的 `nginx -V` **应当完全一致**。不一致 → 告警。

> 这是很实际的坑：你在一台上重新编译加了模块，忘了另一台，配置同步过去直接 `nginx -t` 失败，reload 卡住。平台把这个差异自动检测出来，比人肉对比强。

**③ 模块清单可视化**
节点详情页展示模块列表，两台并排对比，差异高亮。

### 6.2 平台要支持的三种配置块

因为你有 `--with-stream`，平台不能只管 `http{}`：

```
nginx.conf
├── events { }                    # 基础
├── http { }                      # 七层（server / upstream / location）
│   └── conf.d/*.conf
├── stream { }                    # 四层（你编译了 stream，必须支持）
│   └── stream.d/*.conf
└── (keepalived.conf 单独管理)
```

**stream 块的实际用途**（配合 `ssl_preread`）：在同一 443 端口上，根据 SNI 把不同域名的 TLS 流量分发到不同后端，且不解密 —— 这在外网只有一个 IP 但有多个 HTTPS 服务时非常有用。平台要能编辑和校验它。

---

## 决策 7 · 安全边界（无 LDAP 对接）

你说暂无对接需求，那权限做最简：

| 项 | v1 做法 |
| --- | --- |
| 账号 | 本地账号，密码 bcrypt/argon2id 存储 |
| 角色 | 3 个内置角色：`admin`（全部）、`operator`（改配置 + 发布，不能改系统设置/用户）、`viewer`（只读） |
| 2FA | **建议开启 TOTP**（自用也要开，因为这个面板能改线上配置，被拿下就是全站沦陷） |
| 登录保护 | 同 IP 连续 5 次失败锁定 15 分钟 |
| 面板暴露 | **不要暴露在公网**。仅监听内网 / 走 Tailscale / WireGuard / SSH 隧道 |
| 审计 | 所有写操作记 `{who, when, what, from_ip, result}`，不可删除 |
| 预留 | 用户表设计成可挂 `external_id` 字段，将来要接 LDAP/OAuth 不用改表 |

---

## 附 A · 最终技术栈定稿

| 层 | 定稿 | 备注 |
| --- | --- | --- |
| **控制面** | Go 单二进制 + 内嵌前端（`embed.FS`）+ systemd | 2C4G / 60GB SSD |
| **Agent** | Go 单二进制（< 20MB），4 台全装 | 含配置下发 + 日志采集 + 合规巡检 + 度量上报 |
| **通信** | gRPC 双向流 + mTLS | Agent 主动外连，节点无需开放入站端口 |
| **数据库** | **SQLite（WAL）** | 单文件备份；预留 PG 迁移 |
| **配置版本** | **SQLite 内容寻址 blob + revision 链路** | 可选导出到 Git 裸仓 |
| **队列/锁** | 进程内 goroutine + SQLite 事务 | 不引入 Redis |
| **日志采集** | **Agent 内置 tail 模块** | 默认开；Vector 作为大流量可选增强 |
| **日志存储** | **ClickHouse 单实例**（限 2GB 内存，TTL 7 天） | 2G 内存机器改用 VictoriaLogs |
| **告警事件** | ClickHouse（永久保留）+ SQLite 镜像 | 安全审计依据，不删 |
| **指标** | Agent 心跳 + `stub_status` → SQLite 时序表 | Grafana 延后到 v0.3 |
| **证书** | Cloudflare DNS-01（ACME）+ 手动上传 | DNSProvider 接口预留 Aliyun |
| **前端** | Vue 3 + Naive UI + Monaco Editor | Monaco 提供 Nginx 语法高亮与 Diff |
| **节点** | Nginx 1.30.0 编译版 × 2，Keepalived × 2 | 能力基线来自 `nginx -V` |

**部署形态**：

```
┌────────────────────────────┐
│  控制面（独立小 VM / 内网）    │
│  ngxcp-server (Go 单二进制)  │
│  ├─ SQLite   (配置/任务/审计) │
│  ├─ ClickHouse (日志 7 天)   │
│  └─ 内嵌 Web UI              │
└─────────────┬──────────────┘
        mTLS 双向流（Agent 主动外连）
   ┌──────────┼──────────┬───────────┐
┌──▼───┐  ┌──▼───┐  ┌───▼──┐  ┌───▼──┐
│ DIR1 │  │ DIR2 │  │ RS1  │  │ RS2  │
│keepa-│  │keepa-│  │nginx │  │nginx │
│lived │  │lived │  │1.30  │  │1.30  │
│MASTER│  │BACKUP│  │      │  │      │
└──────┘  └──────┘  └──────┘  └──────┘
   └── VIP 10.0.0.100 (DR 模式调度) ──┘
```

---

## 附 B · 修订后的里程碑（按 2+2 规模重排）

原 12 周 → **压缩到 7 周**（单人全职或半职 10–12 周）。

| 阶段 | 周期 | 交付 | 验收 |
| --- | --- | --- | --- |
| **M1 骨架** | W1 | Go 控制面骨架 + SQLite schema + Agent 注册/心跳/mTLS + 概览页 | 4 台全部在线，掉线 30s 内感知 |
| **M2 配置闭环** | W2–W3 | 配置浏览/编辑（Monaco）/版本/diff + `nginx -t` 校验 + 能力基线检测 | 能改一个 conf、看到 diff、看到校验结果；故意写错被拦下 |
| **M3 发布引擎** | W4–W5 | 原子落盘 + 快照 + **LVS 权重摘除式无损发布** + 自动回滚 + 漂移检测 | 发一个错误配置 → 零节点被污染 → 自动回滚；RS 摘除期间零 5xx |
| **M4 证书 + LVS** | W6 | Cloudflare DNS-01 自动签发 + 手动上传校验 + 分发；Keepalived 双机渲染 + DR 合规巡检 + 拓扑图 | 一张测试证书自动续期并分发；手工改坏 arp_ignore 能被检出并阻断发布 |
| **M5 日志 + 预警** | W7 | Agent 日志采集 + ClickHouse + 检索页 + TraceID 追踪 + 攻击规则 + 封禁变更单 | 输入一个 request_id 能查到全链路；注入特征触发自动封禁并可一键回滚 |

> M5 之后（v0.3+）：Grafana 指标、审批流、保存的查询、GeoIP 可视化、多 DNS Provider。

---

## 附 C · 三个仍需你确认的点

1. **拓扑确认**：Keepalived 是 2 台独立 Director，还是与 2 台 Nginx 同机部署（共 2 台）？↳ 影响 DR 的 ARP 处理方式和拓扑图渲染
2. **控制面部署位置**：独立小主机 / 复用现有机器 / 本地 Mac 上跑？↳ 若 Agent 需要从公网连回，控制面必须有固定入口（域名或 Tailscale）
3. **日志量级**：日 PV 大概多少？↳ 决定上 ClickHouse 还是先用轻量方案（见 3.7）

---

# 第二轮决策（基于真实环境校准）

> 环境基线更新（2026-09-03）：
> - 生产：**2 台 Keepalived Director（独立物理/虚拟节点）+ 2 台 Nginx RS**，DR 模式，nginx 1.30.0 编译安装
> - 本地：**2 台 TH-D2110**，每台 Xeon Gold 6330（112 逻辑核 / 128G 内存 / 25T 存储）
> - DNS：Cloudflare（历史：阿里云）
> - 目标量级：**百万级访问**
> - 开发模式：**全程 AI 编写**

---

## 决策 8 · 容量：百万级访问，2 LVS + 2 Nginx 够不够？

### 8.1 先把"百万级"翻译成工程单位

"百万级"这个说法必须先量化，否则结论没有意义。按最常见的口径——**日 PV 100 万**——拆解：

| 项 | 计算 | 结果 |
| --- | --- | --- |
| 平均 QPS | 1,000,000 ÷ 86,400 | **11.6 QPS** |
| 峰值 QPS | 按日均 8 倍峰谷比 | **93 QPS** |
| 峰值请求数 | 每 PV 平均 8 个 HTTP 请求（含 CSS/JS/图片） | **744 req/s** |
| 日均带宽 | 每 PV 300KB（静态未上 CDN 的保守值） | 300 GB/天 ≈ **27.7 Mbps** |
| 峰值带宽 | 10 倍 | **277 Mbps** |
| 峰值 PPS | 744 req/s × 约 15 包/请求 | **约 11,000 PPS** |

> 如果你的"百万级"指的是**日请求 1000 万**（不是 PV），把上面所有数乘 10：峰值 **7440 req/s**、带宽 2.8 Gbps。下面会分档讨论。

### 8.2 逐层对照瓶颈

| 层 | 单机能力（保守值） | 你的峰值负载 | 余量 |
| --- | --- | --- | --- |
| **LVS Director（DR 模式）** | CPS 数十万级；DR 模式**回包不走 Director**，只处理入站包，吞吐天花板极高 | 744 req/s | **> 100 倍** |
| **LVS 网卡 PPS** | 千兆 ~1.4M PPS，万兆 ~14.8M PPS | 11,000 PPS | **> 100 倍** |
| **Nginx RS（反代动态）** | 5,000–20,000 QPS（取决于 upstream 延迟与连接复用） | 372 req/s（2 台均摊） | **15–50 倍** |
| **Nginx RS（纯静态）** | 单 worker 数万 QPS | 同上 | **> 50 倍** |
| **网络带宽** | 千兆有效 ~940 Mbps；万兆 ~9.4 Gbps | 277 Mbps | **3 倍（唯一需要盯的）** |

**结论：够用，余量 1–2 个数量级。**

而且 LVS 那台几乎不可能成为瓶颈 —— 这是 DR 模式的本质优势：请求包进 Director 改 MAC 转发，响应包由 RS 直接回客户端，**Director 只承担单向流量**。这也是为什么 DR 是生产首选。

### 8.3 但真正的问题不是"平时够不够"，是"坏一台够不够"

这是我更想强调的一点。**容量评估的正确问法是：N+1 冗余下，任意一台宕机，剩下的能不能扛住全量峰值？**

按上面的数据：坏一台 Nginx，剩一台承担 744 req/s vs 单机 5,000–20,000 QPS 能力 —— **依然有 7–27 倍余量，安全**。坏一台 Director，Keepalived 主备切换 —— **无感**。

所以这套架构的容量结论是：**稳，而且是"坏一台还能扛"的那种稳**。

但这条结论有个前提必须写进运维规范：

> **单台 Nginx 必须能扛住 100% 峰值流量 + 30% 缓冲，否则 2 台就不是冗余，而是"两个半台"。**

平台要做的事：
- **压测基准入库**：记录每台 RS 实测 QPS 上限（用 `wrk`/`ab` 定期跑，Agent 上报）
- **容量水位看板**：峰值 QPS ÷ 单机基准，超过 **40%** 告警（意味着坏一台就到 80%，危险）
- **扩容动线**：加一台 RS = 分钟级（后面 §8.5 说）

### 8.4 什么时候会真的不够：按量级分档

| 阶段 | 量级 | 瓶颈最先出现在 | 动作 |
| --- | --- | --- | --- |
| **L1（当前）** | 日 PV ≤ 100 万，峰值 < 1,000 req/s | 带宽（静态未上 CDN 时） | 现状即可。**建议静态上 CDN/OSS**，把带宽压力挪走 |
| **L2** | 日 PV 100–1000 万，峰值 1k–10k req/s | 后端应用与数据库先于 Nginx 到顶 | 静态上 CDN；**Nginx RS 扩到 4–8 台**（LVS 加 RS 即可，Director 不动）；后端加缓存 |
| **L3** | 峰值 10k–50k req/s | 单机房带宽 / 单机 PPS | 万兆网卡 + bonding；RS 扩到 8–16 台；**LVS Director 从主备改 ECMP**（OSPF/BGP 等价多路径，Director 横向扩到 N 台） |
| **L4** | 峰值 > 50k req/s 或需异地容灾 | 单机房物理上限 | DNS GSLB 或 **Anycast** 多机房；每机房一套 LVS+Nginx |

**关键判断：以你的百万 PV 目标，卡在 L1/L2，L3 遥遥无期。**

所以架构上真正该投资的是：
1. **让"加一台 RS"变成低成本操作** —— 这正是本平台的核心价值
2. **静态资源上 CDN** —— 一笔钱解决最大的带宽瓶颈
3. **容量数据可观测** —— 知道离天花板还有多远，比提前扩容重要

### 8.5 扩容路径（与平台能力对齐）

```
L1: 2 RS  ── 加 RS ──> L2: 4~8 RS  ── 上 CDN ──> 带宽不再是瓶颈
                                    └── 后端扩容 ──> 应用/DB 瓶颈
L3: 单机房产顶 ──> LVS Director 改 ECMP（主备 → 多活）
L4: 异地 ──> Anycast / GSLB
```

对应平台要预留的能力（现在不用做，但别设计死）：
- RS 是**节点池**概念，不是硬编码 2 台 —— 数据库里就是 `node` 表的一行
- LVS 的 Director 组当前是 `mode=active-standby`，将来扩展 `mode=ecmp` —— **在 `lvs_cluster` 表里留一个字段**，不要到处写死主备逻辑
- 健康检查与权重操作统一走 `ipvsadm` 抽象层

### 8.6 一个容易被忽略的点：LVS-DR 的物理约束

DR 模式要求 **Director 与所有 RS 必须在同一二层网络**（同网段 / 同 VPC），因为靠改 MAC 地址转发。

这意味着：
- **不能跨机房/跨可用区做 DR** —— 这是 DR 唯一的硬伤
- 云厂商 VPC 内一般可用（同 VPC 同子网）
- 真要跨机房，得改 **TUN/IPIP 模式** 或 **NAT 模式**，或者在每个机房各部署一套

你现在是自建物理机，同机房二层没问题。**但如果将来要扩展，这个约束会最先撞墙** —— 提前记在架构文档里，避免到时候推倒重来。

---

## 决策 9 · SQLite vs PostgreSQL —— 修正我上一轮的结论

### 9.1 先认错：我把"单文件 cp 走天下"的权重给高了

上一轮我把"单文件备份"列为 SQLite 的**决定性优势**，现在看有三个问题：

**问题一：SQLite 的 `cp` 备份根本不安全。**

WAL 模式下直接 `cp` 会拿到不一致的快照（WAL 文件与主库文件不是同一时刻的状态）。正确做法只有三种：

```bash
# 正确姿势 1：官方 backup API
sqlite3 /var/lib/ngxcp/ngxcp.db ".backup '/backup/ngxcp.db'"
# 正确姿势 2：VACUUM INTO（SQLite 3.27+，产出的是紧凑且一致的副本）
sqlite3 /var/lib/ngxcp/ngxcp.db "VACUUM INTO '/backup/ngxcp.db'"
# 正确姿势 3：停写后 cp（生产不可用）
```

所以"cp 走天下"这句话**本身就是错的**。真要备份，两边都得写脚本、都得定时、都得验证可恢复。既然如此，这个优势就缩水成了"备份脚本稍微简单一点"。

**问题二：我低估了 PITR（任意时间点恢复）的价值。**

对配置管理平台，真实事故长这样：

> 昨天下午 3 点 12 分，有人误删了 `api-gateway.conf` 里的一个 upstream 块，配置版本被覆盖了。今天上午才发现。
> 需要：**把配置库恢复到昨天下午 3 点 11 分的状态，但保留之后所有的审计日志。**

- **PG**：`pg_basebackup` + WAL 归档 → 恢复到任意秒；或者逻辑备份 + WAL 也能做
- **SQLite**：只能恢复到"最近一次 backup 的时刻"，中间的增量全丢

这个能力差异在**配置管理**场景（数据小、价值高、误操作代价大）里非常实际。

**问题三：你的资源约束不成立。**

上一轮我按"控制面 2C4G 单实例"推的。实际是 **128G 内存 / 25T 存储 / 112 核** —— PG 跑起来毫无压力，"省资源"这个理由自动失效。

### 9.2 重新对比（在"资源管够"前提下）

| 维度 | SQLite | PostgreSQL | 谁赢 |
| --- | --- | --- | --- |
| 备份一致性 | 需 backup API / VACUUM INTO | `pg_dump`（逻辑）/ `pg_basebackup`（物理） | 平 |
| **任意时间点恢复** | ✗ 不支持 | ✓ WAL 归档 PITR | **PG** |
| 并发写 | 单写者，写锁串行 | MVCC，行级锁 | **PG**（规模扩大后才显现） |
| 半结构化查询 | JSON1 扩展，能力有限 | **JSONB + GIN 索引**，可索引、可查询、可更新 | **PG** |
| 全文检索 | FTS5 可用但调试烦 | `tsvector` + 内置分词；中文需扩展 | 平 |
| 运维成本 | **零**（无进程、无端口、无用户、无升级） | 需部署/监控/升级/调优 | **SQLite** |
| 远程访问 | ✗ 不支持 | ✓ 可跨机、可主从 | **PG** |
| 生态（Grafana 等直连） | ✗ | ✓ 一堆工具原生支持 | **PG** |
| 与 ORM/迁移工具配合 | 一般 | **成熟**（golang-migrate / Atlas / ent） | **PG** |
| 启动速度 / 本地开发 | **极快**，零依赖 | 需要起服务（Docker 一行） | **SQLite** |

### 9.3 结论：主用 PostgreSQL，SQLite 降级为开发态

**选 PG。理由是三条具体的，不是"PG 更好"这种废话：**

1. **PITR** —— 配置管理平台的误操作恢复是刚需，SQLite 给不了
2. **JSONB + GIN** —— 节点能力清单、配置解析结果、审计详情、安全事件证据全是半结构化数据，PG 能索引能查，SQLite 只能存文本
3. **你资源管够 + AI 写代码** —— PG 的部署与 schema 迁移成本，对 AI 来说就是几行 Docker Compose 和一个 migration 文件，边际成本趋近于零

**但保留 SQLite 作为开发态与单机降级模式：**

```bash
# 开发/演示：零依赖，克隆下来就能跑
NGXCP_DB_DRIVER=sqlite NGXCP_DB_DSN=/tmp/ngxcp.db ./ngxcp-server

# 生产：PostgreSQL
NGXCP_DB_DRIVER=postgres NGXCP_DB_DSN="postgres://ngxcp:xxx@127.0.0.1:5432/ngxcp?sslmode=disable"
```

实现方式：**用 ent 或 GORM 做 ORM 抽象屏蔽方言差异**，业务代码不写裸 SQL。这样切换只改 DSN。

> **为什么推荐 ORM 而不是 sqlc**：AI 全程编写的场景下，ORM 的 schema-as-code 特性让 AI 更容易一次性生成正确代码（不用手写 SQL、不用管 scan 顺序、类型安全由生成代码保证）。sqlc 更严谨但要求 AI 先写 SQL 再生成，多一轮往返，出错面更大。

**推荐 ent**（比 GORM 更类型安全，schema 定义即图模型，自动生成 CRUD + migration，AI 友好）。折中可选 GORM（生态更大，AI 训练数据更多，出问题更容易搜到答案）。

我倾向 **ent**，理由是这个项目的实体关系（Node ← ConfigFile ← ConfigRevision ← ChangeOrder ← DeployTask）是典型的图模型，ent 的边（Edge）建模天然契合，而且 ent 的 migration 是自动生成的，AI 不用手写 SQL 迁移。

### 9.4 PG 部署建议（资源管够，但仍要设上限）

```yaml
# docker-compose.yml 片段
services:
  postgres:
    image: postgres:18-alpine
    environment:
      POSTGRES_DB: ngxcp
      POSTGRES_USER: ngxcp
      POSTGRES_PASSWORD: ${PG_PASSWORD}
    command:
      - postgres
      - -c shared_buffers=2GB          # 128G 机器，2G 足够这个负载
      - -c effective_cache_size=8GB
      - -c work_mem=32MB
      - -c max_connections=100
      - -c wal_level=replica           # 开启 WAL 归档以支持 PITR
      - -c archive_mode=on
      - -c archive_command='test ! -f /wal_archive/%f && cp %p /wal_archive/%f'
      - -c log_min_duration_statement=500   # 慢查询 > 500ms 记日志，AI 排查友好
    volumes:
      - pgdata:/var/lib/postgresql/data
      - ./wal_archive:/wal_archive
    deploy:
      resources:
        limits: { memory: 4G, cpus: '4' }   # 硬限制，防止失控
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ngxcp"]
      interval: 10s
```

**备份策略（写一个 `scripts/backup.sh`，cron 每天跑）：**

```bash
#!/usr/bin/env bash
set -euo pipefail
TS=$(date +%Y%m%d-%H%M%S)
DEST=/backup/ngxcp/$TS
mkdir -p "$DEST"

# 1. 全局逻辑备份（跨版本可恢复，可读 SQL）
docker exec ngxcp-postgres pg_dump -U ngxcp -Fc ngxcp > "$DEST/ngxcp.dump"

# 2. 配置快照目录（Agent 上传的发布前快照）
tar czf "$DEST/snapshots.tar.gz" -C /var/lib/ngxcp snapshots/

# 3. 密钥（Agent CA 私钥、master key、CF token 加密材料）—— 单独加密打包
tar czf - -C /etc/ngxcp secrets/ | \
  openssl enc -aes-256-cbc -pbkdf2 -pass file:/etc/ngxcp/backup.pass \
  -out "$DEST/secrets.tar.gz.enc"

# 4. 构建产物（nginx 二进制与模块）
tar czf "$DEST/artifacts.tar.gz" -C /var/lib/ngxcp/artifacts/ .

# 5. 保留 30 天
find /backup/ngxcp -maxdepth 1 -type d -mtime +30 -exec rm -rf {} +

# 6. 异地同步（有第二台机器就直接 rsync 过去）
rsync -a --delete /backup/ngxcp/ backup-host:/backup/ngxcp/
```

> 关键点：**备份必须包含 `secrets/`**。丢了 Agent CA 私钥，所有 Agent 要重新注册 —— 这是最容易被忽略的灾难场景。

---

## 决策 10 · 时序数据库：不单独引入，但要分清两类数据

### 10.1 先把数据分类，别笼统说"时序"

这个平台有四类存储需求，访问模式完全不同，**混在一个库里必然四不像**：

| 数据类型 | 量级估算（百万 PV/天） | 访问模式 | 正确归属 |
| --- | --- | --- | --- |
| **访问日志** | 500 万–1000 万条/天，原始 2.5–5 GB/天 | 写多读少；多维筛选 + 正则 + 分组聚合 | **ClickHouse**（列存，压缩 10–20×） |
| **指标时序** | 4 节点 × 约 60 指标 × 15s 间隔 ≈ 140 万点/天 | 固定 schema；区间聚合、趋势、分位数 | **VictoriaMetrics**（时序压缩 + PromQL） |
| **安全事件** | 几十–几百条/天 | 小量；需长期留存、需关联查询 | **PostgreSQL** |
| **审计日志** | 几百条/天 | 极小；**必须永久保留**、不可篡改 | **PostgreSQL**（append-only） |
| **配置版本** | 几十 KB/次，每天几次 | 极小；需血缘与 diff | **PostgreSQL** |
| **Agent 实时状态** | 4 节点 × 心跳 | 只看当前值 | **内存 + PG 现状表** |

### 10.2 回答"时序数据库要引入么"

**不需要引入独立的时序数据库产品**（InfluxDB / TDengine / IoTDB 之类），理由：

1. **VictoriaMetrics 本身就是时序数据库** —— 再引入一个是重复建设
2. **量级根本用不上专门的时序库**：140 万点/天，压缩后约 30–50 MB/天。这个量 SQLite 都扛得住，更别说专业时序库
3. **多一个组件 = 多一份运维 + 多一份 AI 要理解的上下文**

**TimescaleDB 要不要？** 不加。虽然"PG + TimescaleDB 一套搞定"听起来很美，但：
- 指标访问模式（PromQL / 区间聚合）和 PG 的 SQL 模式不同，TimescaleDB 的 continuous aggregate 比 Grafana+PromQL 生态差
- 多一个扩展就多一个升级兼容负担（PG 大版本升级时扩展经常掉链子）
- 指标挂了不该影响配置库 —— **故障隔离**更重要

### 10.3 存储容量核算（你的 25T 完全够）

| 组件 | 日增量（压缩后） | 保留策略 | 占用 |
| --- | --- | --- | --- |
| ClickHouse（访问日志） | 250–500 MB/天 | **90 天** | **~30 GB** |
| VictoriaMetrics（指标） | 30–50 MB/天 | **1 年** | **~20 GB** |
| PostgreSQL（元数据+审计+事件） | < 5 MB/天 | 永久 | ~2 GB/年 |
| 配置快照 | ~50 MB/次发布 | 90 天 / 200 份 | ~10 GB |
| 构建产物（nginx 二进制） | ~200 MB/版本 | 保留最近 10 版 | ~2 GB |
| 备份（dump + 快照 + 密钥） | 滚动 30 天 | — | ~50 GB |
| **合计** | | | **~115 GB / 25 TB（0.46%）** |

**结论：存储完全不是约束，可以放开保留期。** 上一轮我按"控制面 60GB SSD"推的 7 天 TTL，现在改成 **90 天**（日志）+ **1 年**（指标），排查历史问题会舒服很多。

---

## 决策 11 · 监控：不要自研，但要自研"业务视角"

### 11.1 明确分工（这是本节最重要的表）

| 能力 | 归属 | 理由 |
| --- | --- | --- |
| 机器指标（CPU/内存/磁盘/网络/IO） | **Grafana + node_exporter** | 成熟、零成本、面板现成 |
| Nginx 性能指标（QPS/连接/状态码/耗时分位） | **Grafana + Agent /metrics** | PromQL 的分位数与速率计算自研很痛苦 |
| LVS 指标（每 VS/RS 的 Conn/PPS/BPS） | **Grafana + Agent /metrics** | 同上 |
| 长期趋势与容量规划 | **Grafana** | 时间缩放、对比、注释都是现成的 |
| 基础设施告警规则与路由 | **vmalert + Alertmanager** | `group_by` / `inhibit` / `silence` 三件套自研至少两周 |
| **集群健康总览** | **自研页面** | 与业务实体（节点/配置/证书）强耦合，Grafana 表达不了 |
| **发布任务实时进度** | **自研页面** | 核心差异化；是审批与回滚的操作入口，不只是看 |
| **配置漂移检测** | **自研** | 需要 diff 与"应用到哪一版"的语义 |
| **证书到期倒计时** | **自研页面** | 要能一键续期，是动作入口 |
| **LVS 拓扑与 RS 权重** | **自研页面** | 要能拖拽摘除/加回，是控制面板不是看板 |
| **日志检索与 TraceID 追踪** | **自研页面** | Grafana/Loki 的日志体验对运维排查不友好 |
| **安全事件与封禁处置** | **自研页面** | 要联动生成变更单走发布流水线 |

**一句话原则：看（observe）用 Grafana，动（act）用自研页面。**

判断标准很简单 —— 如果某个视图的最终目的是"让人点一下做什么"，就自研；如果只是"看趋势找异常"，就用 Grafana。

### 11.2 Prometheus vs VictoriaMetrics：选 VictoriaMetrics 单实例

| 维度 | Prometheus | **VictoriaMetrics（单实例）** |
| --- | --- | --- |
| 部署 | 单二进制 | **单二进制，无依赖** |
| 内存占用 | 每百万活跃时序 4–8 GB | **0.5–1 GB**（约 1/5） |
| 本地保留期 | 默认 15 天，拉长代价高 | `-retention.period=1y` 直接配，有 25T 随便存 |
| 查询语言 | PromQL | **MetricsQL**（兼容 PromQL + 扩展） |
| Grafana 兼容 | 原生 | **原生（数据源选 Prometheus 即可）** |
| 生态 exporter | 全部可用 | **全部可用（同样 scrape /metrics）** |
| 集群扩展 | Thanos / Cortex，复杂 | `vmcluster` 内置，路径清晰 |
| 文档与 AI 熟悉度 | 最好 | 好 |

**选 VictoriaMetrics。** 决定性的三条：单二进制零依赖、内存占用只有 1/5、保留期随意配（你有 25T，指标存一年对容量规划极有价值）。而且因为它兼容 PromQL，**所有 Prometheus 生态的 exporter 和 Grafana 面板直接复用，零迁移成本**。

### 11.3 采集方式：Agent push，不走 scrape

```
Agent ──remote_write(Prometheus 文本格式)──> VictoriaMetrics :8428/api/v1/write
  │
  └── gRPC 心跳 ──> 控制面（判定在线/离线）
```

**为什么用 push 不用 pull**：
- Agent 是主动外连模式（mTLS 出向），VictoriaMetrics **不需要知道节点 IP**，NAT 后面也能工作
- 控制面统一管理采集配置（采集间隔、指标白名单、采样率），改配置不用改 VM 的 scrape 配置
- 节点存活判定走 gRPC 心跳（30 秒内感知），**不依赖 scrape 成功率** —— 职责分离更清晰

**指标来源（关键设计：不依赖 nginx 编译额外模块）**：

你的 nginx 编译参数里有 `--with-http_stub_status_module`，但**没有** `nginx-module-vts`（第三方模块，需要重新编译）。指标方案因此分三层：

| 层 | 来源 | 提供的指标 |
| --- | --- | --- |
| **L1 基础**（无需额外模块） | `stub_status` | Active / Accepts / Handled / Requests / Reading / Writing / Waiting |
| **L2 业务**（**Agent 从 access log 滑窗聚合**，推荐主力） | Agent goroutine | QPS、状态码分布、P50/P95/P99 耗时、`upstream_addr` 分布、`upstream_response_time`、Top URI/IP/UA |
| **L3 系统** | Agent 读 `/proc` + `ipvsadm` | CPU/内存/磁盘/网络/文件描述符；LVS 每 VS/RS 的 Conn/PPS/BPS |

**L2 是核心创新点**：Agent 边采集日志边做滑窗聚合（60 秒窗口），既产出指标又产出日志，**口径天然一致**。好处：
- nginx 不用重新编译，不引入第三方模块风险
- 指标维度比 VTS 还灵活（想加什么字段就加，因为是自己的代码）
- 与日志检索的数据源同源，排查时"图上看到尖峰"和"日志里查到慢请求"是同一份数据

`stub_status` 只作为交叉校验（验证 Agent 聚合是否准确）。

> 要启用 stub_status，需要配置里加：
> ```nginx
> server {
>     listen 127.0.0.1:8080;
>     location /nginx_status {
>         stub_status;
>         allow 127.0.0.1;
>         deny all;
>     }
> }
> ```
> 平台可以一键下发这个片段（走正常发布流水线）。

### 11.4 Alertmanager 的价值：别自研告警路由

Alertmanager 有三个自研极其痛苦的能力：

```yaml
# ① group_by：同一集群的 20 个告警合并成一条通知，不是 20 条
route:
  group_by: ['cluster', 'alertname']
  group_wait: 30s       # 等 30 秒，看有没有同类告警一起发
  group_interval: 5m
  repeat_interval: 4h   # 4 小时内不重复打扰

# ② inhibit：节点宕机时，抑制该节点上的所有其他告警（否则告警风暴）
inhibit_rules:
  - source_match: { alertname: NodeDown }
    target_match_re: { node: ".*" }
    equal: ['node']

# ③ silence：维护窗口期静默，带过期时间
```

这三个自研至少要两周，且很难做对。**直接用。**

**告警统一收敛到平台**：

```
vmalert ──> Alertmanager ──> Webhook ──> 控制面 /api/v1/alerts/ingest
                                              │
平台自研告警（漂移/证书/发布失败/安全事件）──┘
                                              │
                                    ┌─────────┴─────────┐
                              平台告警中心（去重/分级/处置状态）
                                              │
                              可一键生成变更单 → 走发布流水线
```

这样运维只需要看一个地方。

### 11.5 Grafana 嵌入方式：Auth Proxy（最干净）

平台内嵌 Grafana 面板，三种方案对比：

| 方案 | 做法 | 问题 |
| --- | --- | --- |
| iframe 直连 | 前端直接 iframe Grafana 地址 | Graph 需登录，登录态不同步 |
| **Auth Proxy（推荐）** | Grafana 只监听 `127.0.0.1`，平台后端反代 `/grafana/`，注入 `X-WEBAUTH-USER` header | 需要配置，但最干净 |
| API Key | 前端带 Grafana API Key | Key 泄露到浏览器，不安全 |

**Auth Proxy 配置**：

```ini
# grafana.ini
[auth.proxy]
enabled = true
header_name = X-WEBAUTH-USER
header_property = username
auto_sign_up = true          # 首次访问自动创建用户
whitelist = 127.0.0.1        # 只信任本机反代，防止外部伪造 header
```

```nginx
# 平台后端反代（Go 里实现，或前置 Nginx）
location /grafana/ {
    proxy_pass http://127.0.0.1:3000/;
    proxy_set_header X-WEBAUTH-USER $ngxcp_user;   # 由平台鉴权中间件注入
}
```

**Grafana 面板用 JSON 文件 provisioning**（`provisioning/dashboards/*.json`），纳入 Git 版本管理 —— 改面板走代码评审，不在 UI 上手改。这对 AI 编写极其友好：面板就是 JSON，AI 能直接生成。

**预置面板清单（AI 生成 JSON）**：
1. `nginx-overview.json` —— QPS / 连接数 / 状态码分布 / P50·P95·P99 / upstream 分布
2. `node-system.json` —— CPU / 内存 / 磁盘 / 网络 / 文件描述符
3. `lvs-dr.json` —— 每 VS 的 Conn/PPS/BPS；每 RS 的权重、活跃连接、摘除状态
4. `keepalived.json` —— VRRP 状态、主备切换历史、脑裂告警
5. `certificates.json` —— 证书到期倒计时、续期成功率

### 11.6 监控组件清单与资源预算

| 组件 | 作用 | CPU | 内存 | 备注 |
| --- | --- | --- | --- | --- |
| VictoriaMetrics | 时序存储 + 查询 | 2C | 2–4G | `-retention.period=1y` |
| vmalert | 告警规则评估 | 共享 | 256M | 规则即 YAML，AI 好写 |
| Alertmanager | 告警路由 | 共享 | 256M | group_by/inhibit/silence |
| Grafana | 可视化 | 1C | 1G | Auth Proxy 嵌入 |
| blackbox_exporter | **外部探活 VIP**（端到端） | 共享 | 64M | 见下 |
| node_exporter | 机器指标（可选，Agent 已覆盖大部分） | 共享 | 64M | 可省，Agent 已做 |

> **blackbox_exporter 值得单独说**：只有从**外部**探测 VIP 才能验证端到端链路（Director → RS → 应用）。Agent 在 RS 上自查只能证明"自己活着"，证明不了"用户能访问"。这是监控里最容易漏的一环。

---

## 决策 12 · nginx 编译升级与模块管理

这是你提的几个需求里**最有工程含量**的一个，也是这个平台最难被替代的能力之一。

### 12.1 为什么这件事难（三个真实的坑）

**坑一：`nginx_upstream_check_module` 需要给 nginx 打补丁。**

它不是一个普通的 `--add-module`，而是要求对 nginx 源码打 patch，且**不同 nginx 版本对应不同的 patch 文件**：

```
nginx_upstream_check_module/
├── check_1.20.1+.patch
├── check_1.16.1+.patch
├── check_1.14.0+.patch
├── check_1.12.1+.patch
├── check_1.11.5+.patch
├── check_1.9.2+.patch
├── check_1.7.5+.patch
├── check_1.7.2+.patch
└── check.patch                 # 兜底
```

选错 patch → 编译失败，或者**编译成功但运行时 upstream check 行为异常**。这个映射关系必须维护成数据。

**坑二：生产机上不该装编译工具链。**

在生产 nginx 机器上装 gcc、make、pcre-devel、openssl-devel、zlib-devel，违反最小化原则，且不同机器编译环境不同 → 产物不可复现。

**坑三：升级过程必须无损。**

nginx 升级有官方的二进制热升级机制（`USR2` → `WINCH` → `QUIT`），但每一步都有失败分支，且回滚时机窗口很窄。手工执行极易出错。

### 12.2 平台设计：构建与升级中心（Build & Upgrade）

```
┌─ 控制面 ────────────────────────────────────────────┐
│ ① 版本矩阵（module_matrix.json）                      │
│    nginx 版本 × 模块版本 → patch 文件 / 兼容性         │
│                                                     │
│ ② 构建器（Builder）                                   │
│    容器化编译 → 产出二进制 + 模块 + buildinfo         │
│                                                     │
│ ③ 产物仓库（Artifacts）                               │
│    /var/lib/ngxcp/artifacts/nginx-<ver>/            │
│    ├── nginx                 # 二进制                │
│    ├── modules/*.so                                 │
│    └── buildinfo.json       # 完整编译参数与依赖版本  │
│                                                     │
│ ④ 升级执行器（复用发布流水线 + LVS 摘除）              │
└─────────────────────────────────────────────────────┘
```

### 12.3 版本兼容矩阵（数据驱动）

```jsonc
// configs/module_matrix.json —— 纳入版本管理，AI 可联网更新
{
  "updated_at": "2026-09-03",
  "modules": {
    "nginx_upstream_check_module": {
      "repo": "https://github.com/yaoweibin/nginx_upstream_check_module",
      "patches": [
        { "min_nginx": "1.20.0", "max_nginx": "1.99.0", "patch": "check_1.20.1+.patch" },
        { "min_nginx": "1.16.1", "max_nginx": "1.19.9", "patch": "check_1.16.1+.patch" },
        { "min_nginx": "1.14.0", "max_nginx": "1.16.0", "patch": "check_1.14.0+.patch" }
      ],
      "notes": "patch 与 nginx 版本必须严格对应；nginx 1.30 应使用 check_1.20.1+.patch"
    },
    "ngx_brotli":      { "type": "add-module", "compat": ">=1.20" },
    "headers-more":    { "type": "add-module", "compat": ">=1.14" },
    "nginx-module-vts":{ "type": "add-module", "compat": ">=1.12" }
  },
  "openssl_matrix": {
    "3.5.x": { "min_nginx": "1.25.0", "note": "nginx 1.25+ 才完整支持 OpenSSL 3.x" }
  }
}
```

**平台行为**：
- 用户选择"nginx 1.30.0 + check module" → 平台自动匹配 patch 文件
- 无匹配 → **直接拒绝并提示**，不让用户瞎试
- 可联网检查上游 release，提示"check module 已支持 nginx 1.32"

### 12.4 构建流程（容器化，保证可复现）

```dockerfile
# docker/build.Dockerfile
FROM rockylinux:9 AS builder
ARG NGINX_VERSION=1.30.0
ARG CHECK_MODULE_REF=master

RUN dnf install -y gcc make pcre-devel zlib-devel openssl-devel perl-ExtUtils-Embed gd-devel

WORKDIR /build
# ① 拉 nginx 源码（校验 hash）
RUN curl -fsSL https://nginx.org/download/nginx-${NGINX_VERSION}.tar.gz -o nginx.tar.gz \
 && echo "${NGINX_SHA256}  nginx.tar.gz" | sha256sum -c - \
 && tar zxf nginx.tar.gz

# ② 拉模块源码
RUN git clone --depth 1 -b ${CHECK_MODULE_REF} \
    https://github.com/yaoweibin/nginx_upstream_check_module.git

# ③ 打补丁（patch 文件由矩阵决定，构建时注入）
COPY ${PATCH_FILE} /tmp/check.patch
RUN cd nginx-${NGINX_VERSION} && patch -p1 < /tmp/check.patch

# ④ configure（参数由平台从「现有能力基线 + 用户调整」渲染）
RUN cd nginx-${NGINX_VERSION} && ./configure ${CONFIGURE_ARGS} && make -j$(nproc)

# ⑤ 产出
RUN mkdir -p /out && cp objs/nginx /out/nginx \
 && mkdir -p /out/modules && cp objs/*.so /out/modules/ 2>/dev/null || true
```

**关键设计：configure 参数从能力基线渲染，不是手写。**

平台从节点的 `nginx -V` 解析出现有参数，让用户在此基础上勾选增删，避免"漏了一个 `--with-http_v3_module` 导致升级后功能缺失"这种经典事故。

```go
// 从 nginx -V 解析 + 用户调整 → 渲染 configure 参数
type BuildSpec struct {
    NginxVersion string
    Prefix       string   // /etc/nginx
    SbinPath     string   // /usr/sbin/nginx
    ConfPath     string   // /etc/nginx/nginx.conf
    Modules      []Module // 静态编译模块清单
    DynamicMods  []Module // 动态模块
    OpenSSL      string   // 系统 OpenSSL 或指定源码路径
    ExtraArgs    []string
}

func (s BuildSpec) Render() string {
    // 输出：./configure --prefix=/etc/nginx --sbin-path=/usr/sbin/nginx ...
}
```

**buildinfo 必须完整记录**（否则三个月后没人知道这台机器上跑的二进制是怎么来的）：

```json
{
  "nginx_version": "1.30.0",
  "build_id": "20260903-114512-a3f9c2",
  "built_at": "2026-09-03T11:45:12Z",
  "builder": "rockylinux:9 / gcc 11.5.0",
  "configure_args": "--prefix=/etc/nginx --sbin-path=/usr/sbin/nginx ...",
  "sources": {
    "nginx": { "url": "...", "sha256": "..." },
    "nginx_upstream_check_module": { "repo": "...", "commit": "a1b2c3d", "patch": "check_1.20.1+.patch" }
  },
  "openssl_version": "3.5.1",
  "binary_sha256": "...",
  "nginx_V_output": "nginx version: nginx/1.30.0 ..."
}
```

### 12.5 热升级执行序列（无损，且可回滚）

nginx 官方二进制升级机制，平台封装成 9 步状态机：

```
初始：老 master (PID 1000) + 老 worker (1001, 1002)

① 备份当前二进制
   cp /usr/sbin/nginx /usr/sbin/nginx.backup-<ts>

② 落新二进制到 staging
   /var/lib/nginx-upgrade/nginx.new   （同分区，保证 rename 原子）

③ 用新二进制校验现有配置（关键：校验必须用新二进制！）
   /var/lib/nginx-upgrade/nginx.new -t -c /etc/nginx/nginx.conf
   ↓ 失败 → 中止，线上零影响

④ rename 替换
   mv /var/lib/nginx-upgrade/nginx.new /usr/sbin/nginx

⑤ 启动新 master（USR2）
   kill -USR2 1000
   → 老 master 重命名 pid 为 .oldbin (1000)，新 master 起 (2000)
   → 此时两套 worker 并存，都在处理请求

⑥ 平滑停止老 worker（WINCH）
   kill -WINCH 1000
   → 老 worker 处理完当前请求后退出
   → 新连接全部由新 worker 处理

⑦ 观测窗口（默认 120s）
   → 检查新 worker 的错误日志增量、探活成功率、QPS 是否恢复
   ↓ 异常 → 立即回滚（见下）

⑧ 确认，退出老 master（QUIT）
   kill -QUIT 1000

⑨ 更新能力基线（重新采集 nginx -V）
```

**回滚（两个窗口，两种姿势）**：

```bash
# 窗口 A：还没 QUIT 老 master（步骤 ⑦ 之前）—— 优雅回滚
kill -HUP 1000        # 老 master 重新拉起 worker
kill -QUIT 2000       # 退出新 master
# 全程不断连接

# 窗口 B：已 QUIT 老 master —— 只能回退二进制
cp /usr/sbin/nginx.backup-<ts> /usr/sbin/nginx
kill -USR2 <新master>  # 用老二进制起新 master
kill -WINCH <新master>
kill -QUIT <新master>
```

**所以观测窗口（步骤 ⑦）的长度是个策略问题**：太短怕没发现异常，太长老 master 一直占资源。默认 **120 秒**，可按变更单配置。

### 12.6 与 LVS 联动：升级也要"先摘后升"

单台 RS 升级的正确顺序（这是 DR 架构的优势）：

```
① ipvsadm -e -t VIP:80 -r RS2:80 -w 0      # 摘除 RS2（新连接不再进来）
② sleep 30                                  # 排空已有连接（按 keepalive 超时定）
③ 确认 RS2 活跃连接数 ≈ 0
④ 执行热升级 9 步
⑤ 双层探活（本地 curl + 控制面远程探活 VIP:权重路径）
⑥ ipvsadm -e -t VIP:80 -r RS2:80 -w 1      # 加回
⑦ 观测 120s（错误率、延迟、QPS）
⑧ 对 RS1 重复 ①–⑦
```

**好处：升级期间用户侧零感知、零 5xx。** 这是 LVS+DR 架构相对"两台 nginx 各自 reload"的核心优势，平台必须把这个能力做出来，否则等于白有 LVS。

### 12.7 平台还要做的两件事

**① 升级前的配置兼容性检查**

nginx 版本升级可能有废弃指令。平台维护一个"指令变更表"：

```jsonc
{
  "removed_in": {
    "1.26.0": ["ssl on"],              // 已废弃，改用 listen ... ssl
    "1.25.0": ["http2 directive"]      // 改用 http2 on;
  },
  "deprecated_in": {
    "1.22.0": ["listen ... http2"]
  }
}
```

升级前扫描配置，命中即**阻断并给出修改建议**。这是最有价值的一步 —— 否则升级到一半发现配置报错，进退两难。

**② 双机一致性校验**

你的 2 台 nginx 应该跑**完全相同**的二进制与模块。平台定时比对两台的 `binary_sha256` 与 `nginx -V`，不一致立即告警。

> 这个场景非常真实：某次手工给一台机器加了模块忘了另一台，配置同步过去就是 502。

---

## 决策 13 · Agent 能力发现：配置目录与日志目录

**直接回答：能，而且是必须做到的第一件事。** 这是整个平台的地基 —— 不先搞清楚"配置在哪、日志在哪"，后面所有功能都是空中楼阁。

### 13.1 三层信息获取

```
层 1：nginx -V        → 编译期路径（prefix / conf-path / sbin-path / log-path / pid-path）
层 2：nginx -T        → 运行期真实配置（含所有 include，且标记了文件边界）  ★ 关键
层 3：文件系统探测     → 日志实际位置、轮转配置、磁盘空间、权限
```

### 13.2 层 1：从 `nginx -V` 解析编译期路径

你的输出：

```
nginx version: nginx/1.30.0
built by gcc 11.5.0 20240719 (Red Hat 11.5.0-11) (GCC)
built with OpenSSL 3.5.1 1 Jul 2025
TLS SNI support enabled
configure arguments: --prefix=/etc/nginx --sbin-path=/usr/sbin/nginx \
  --conf-path=/etc/nginx/nginx.conf --pid-path=/var/run/nginx.pid \
  --lock-path=/var/run/nginx.lock --error-log-path=/var/log/nginx/error.log \
  --http-log-path=/var/log/nginx/access.log --user=nginx --group=nginx \
  --with-threads --with-file-aio --with-http_ssl_module --with-http_v2_module \
  --with-http_v3_module --with-http_realip_module --with-http_stub_status_module \
  --with-http_gzip_static_module --with-stream --with-stream_ssl_module \
  --with-stream_ssl_preread_module --add-module=../nginx_upstream_check_module
```

解析结果（结构化入库）：

```json
{
  "version": "1.30.0",
  "compiler": "gcc 11.5.0",
  "openssl_version": "3.5.1",
  "tls_sni": true,
  "paths": {
    "prefix":        "/etc/nginx",
    "sbin_path":     "/usr/sbin/nginx",
    "conf_path":     "/etc/nginx/nginx.conf",
    "pid_path":      "/var/run/nginx.pid",
    "lock_path":     "/var/run/nginx.lock",
    "error_log":     "/var/log/nginx/error.log",
    "http_log":      "/var/log/nginx/access.log"
  },
  "user": "nginx",
  "group": "nginx",
  "static_modules": [
    "http_ssl", "http_v2", "http_v3", "http_realip",
    "http_stub_status", "http_gzip_static",
    "stream", "stream_ssl", "stream_ssl_preread",
    "upstream_check_module"     // 第三方，从 --add-module 路径提取目录名
  ],
  "dynamic_modules": [],        // 需从配置里 load_module 指令补
  "configure_args_raw": "--prefix=/etc/nginx ..."
}
```

**注意第三方模块的识别**：`--add-module=../nginx_upstream_check_module` 的路径是编译机上的相对路径，运行时没意义。Agent 要从**路径的最后一段**提取模块名，再与已知模块清单比对归一化。

### 13.3 层 2：用 `nginx -T` 拿到完整配置树 ★

**这是最优雅的一步。`nginx -T` 的输出自带文件边界标记**：

```
# configuration file /etc/nginx/nginx.conf:
user  nginx;
worker_processes  auto;
...
http {
    ...
    include /etc/nginx/conf.d/*.conf;
}

# configuration file /etc/nginx/conf.d/api-gateway.conf:
upstream api_backend {
    server 10.0.1.11:8080;
    check interval=3000 rise=2 fall=3 timeout=1000;
}
server {
    listen 80;
    access_log /var/log/nginx/api-gateway.access.log main;
    ...
}

# configuration file /etc/nginx/conf.d/ssl.conf:
...
```

**正则 `# configuration file (.+):$` 就能切分出完整配置树** —— 不用自己处理 `include` 通配符和递归，nginx 已经帮你做完了。

```go
var reConfFile = regexp.MustCompile(`(?m)^# configuration file (.+):$`)

func ParseConfigTree(dump string) []ConfigFile {
    var files []ConfigFile
    matches := reConfFile.FindAllStringSubmatchIndex(dump, -1)
    for i, m := range matches {
        path := dump[m[2]:m[3]]
        start := m[1]
        end := len(dump)
        if i+1 < len(matches) {
            end = matches[i+1][0]
        }
        content := strings.TrimSpace(dump[start:end])
        files = append(files, ConfigFile{
            Path:   path,
            Content: content,
            SHA256: sha256hex(content),
        })
    }
    return files
}
```

**边界情况（AI 实现时必须处理）**：

| 情况 | 处理 |
| --- | --- |
| `nginx -T` 需要读配置权限 | Agent 以 root 运行；非 root 时提示并降级 |
| 配置有语法错误时 `-T` 会失败 | **这是好事** —— 检出语法错误本身就是能力。标记节点为 `config_invalid`，阻断发布 |
| `include` 了不存在的文件 | `-T` 报错，同上处理 |
| 配置里有 `ssl_certificate_key` 等敏感路径 | 只记录路径，**不读取内容** |
| 输出可能很大（几十 KB–几 MB） | 正常，全量哈希后入库（内容寻址去重） |

### 13.4 层 3：从配置中提取日志路径

`nginx -V` 给的是**默认**日志路径，实际可能被 `access_log` / `error_log` 指令覆盖（可以在 `http` / `server` / `location` 任意层级）。所以必须**从 `nginx -T` 的完整输出里提取所有日志指令**：

```go
// 提取所有 access_log / error_log 目标
// 处理形态：
//   access_log /var/log/nginx/access.log main;
//   access_log /var/log/nginx/access.log main buffer=32k flush=5s;
//   access_log off;                              → 跳过
//   access_log syslog:server=10.0.1.5:514;       → 标记为远端，不采集
//   error_log /var/log/nginx/error.log warn;
```

提取后去重 → 得到有效日志文件清单 → 交给采集模块。

**采集模块必须处理的日志细节**：

| 问题 | 处理 |
| --- | --- |
| **日志轮转（logrotate）** | 监控 inode 变化；轮转后重新打开新文件；`copytruncate` 模式要检测文件大小骤降 |
| **轮转期间的丢失** | 轮转后立即扫一遍旧文件尾部（不然后几分钟的日志丢了） |
| **断点续传** | offset 持久化到本地（文件 + offset + inode），重启 Agent 不重采不漏采 |
| **断连补传** | 本地磁盘队列，默认保留 24h / 1GB，超限丢弃最旧的 |
| **多文件并行** | 每个日志文件一个 goroutine；注意 `error.log` 也要采（错误日志是发布探活的关键判据） |
| **采样降载** | 单节点速率超阈值（如 5000 条/秒）自动降到 1/10，并告警 |

### 13.5 完整的 Agent 能力清单

除了上面三层，Agent 还要采集（这些决定平台能做什么、不能做什么）：

| 类别 | 采集项 | 用途 |
| --- | --- | --- |
| **Nginx** | 二进制路径、版本、编译参数、模块清单、进程 PID、worker 数、配置文件树、日志清单 | 能力基线、校验、升级 |
| **进程管理** | 是否 systemd 托管（`systemctl status nginx`）、pid 文件内容、启动用户 | reload / 升级的执行方式 |
| **日志运维** | logrotate 配置是否存在、日志分区剩余空间、日志增长率 | 容量告警、采集可行性 |
| **系统** | OS 发行版与版本、内核版本、SELinux 状态、ulimit、时区、NTP 同步状态 | 兼容性判断、时间对齐（日志排查的基础） |
| **网络** | 网卡与 IP、监听端口、VIP 是否在 lo（DR 合规） | 拓扑、DR 合规 |
| **LVS 角色** | `ipvsadm` 是否存在、`keepalived` 是否存在、当前 VS/RS 表 | 角色自动识别 |
| **DR 合规** | `arp_ignore` / `arp_announce` / `rp_filter`、VIP 是否在物理网卡 | 决策 4 的 6 项硬约束 |
| **文件系统** | `/etc/nginx` 与 staging 目录是否同分区（**决定 rename 能否原子**） | 原子落盘可行性 ★ |

最后一条特别重要：**如果 staging 目录与 `/etc/nginx` 不在同一分区，`rename` 不是原子的，必须降级为"copy + 校验 + 切换"并加文件锁**。Agent 上线自检时必须报出这一点。

### 13.6 节点角色自动识别

```go
func DetectRole(c Capability) NodeRole {
    hasNginx     := c.NginxBinary != ""
    hasKeepalived := c.KeepalivedBinary != ""
    hasIPVS      := c.IPVSAdmPath != ""

    switch {
    case hasNginx && hasKeepalived && hasIPVS: return RoleDirectorAndRS  // 同机部署
    case hasKeepalived && hasIPVS:             return RoleDirector       // 你的 2 台 Director
    case hasNginx:                             return RoleRealServer     // 你的 2 台 Nginx
    default:                                   return RoleUnknown
    }
}
```

角色决定 Agent 能执行哪些指令 —— 符合上一轮定的安全红线（Agent 不提供任意命令执行）：

| 角色 | 允许的操作 |
| --- | --- |
| `real_server` | 配置读写、`nginx -t`、reload、日志采集、证书落盘、二进制热升级、DR 合规自检 |
| `director` | keepalived 配置读写、ipvsadm 权重操作、VRRP 状态上报、脑裂探测 |
| `unknown` | **只允许上报能力，不允许任何写操作** |

---

## 决策 14 · 本地资源预算与部署拓扑

### 14.1 你的硬件余量：极度宽裕

2 台 TH-D2110，每台 **Xeon Gold 6330（112 逻辑核）/ 128G 内存 / 25T 存储**。

对照一下：这套配置跑 2 个 nginx 节点 + 2 个 keepalived，属于**杀鸡用牛刀**（但作为本地全环境宿主，非常合理）。

推测你的实际布局是：2 台物理机跑虚拟化，上面起 4 个节点（2 Director + 2 RS）+ 控制面全家桶。

### 14.2 推荐部署拓扑

```
┌─ 物理机 A（TH-D2110 · 112C / 128G / 25T）────────────────┐
│                                                          │
│  ┌─ 控制面区（Docker Compose）───────────────────────┐   │
│  │ ngxcp-server (systemd, 2C/2G)                     │   │
│  │ PostgreSQL 18        (4C/4G)                      │   │
│  │ ClickHouse           (4C/8G)                      │   │
│  │ VictoriaMetrics      (2C/4G)                      │   │
│  │ Grafana + Alertmanager + vmalert (1C/2G)          │   │
│  └──────────────────────────────────────────────────┘   │
│                                                          │
│  ┌─ 业务节点区（VM 或 LXC）─────────────────────────┐   │
│  │ VM: director-01  (Keepalived + ipvsadm, 2C/2G)    │   │
│  │ VM: rs-nginx-01  (nginx 1.30, 4C/4G)              │   │
│  └──────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────┘

┌─ 物理机 B（同规格）──────────────────────────────────────┐
│  ┌─ 业务节点区 ─────────────────────────────────────┐    │
│  │ VM: director-02  (Keepalived 备, 2C/2G)           │    │
│  │ VM: rs-nginx-02  (nginx 1.30, 4C/4G)              │    │
│  └─────────────────────────────────────────────────┘    │
│  ┌─ 备份与灾备区 ───────────────────────────────────┐    │
│  │ rsync 接收机 A 的备份（dump/WAL/快照/密钥/产物）   │    │
│  │ 影子集群（可选）：配置变更的演练环境                │    │
│  └─────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────┘
```

**关键原则：控制面与业务节点分离，但与 director / rs 不共宿主机。**

原因：控制面挂了业务照跑（Agent 有本地缓存，配置已落盘）；但物理机 A 整机故障时，会同时失去 director-01 + rs-nginx-01 + 控制面 —— 这是**可接受的**（director-02 + rs-nginx-02 仍可承载全量，见 §8.3），但控制面要能在 B 机上快速拉起（备份已在 B 机上）。

> 如果想更稳，控制面可以做成**双机冷备**：A 机的 `/var/lib/ngxcp` + PG 数据目录用 DRBD/rsync 实时同步到 B 机，B 机上准备好 docker-compose，故障时一条命令拉起。

### 14.3 资源预算表

| 组件 | CPU | 内存 | 磁盘 | 硬限制方式 |
| --- | --- | --- | --- | --- |
| **控制面 ngxcp-server** | 2C | 2G | 10G | systemd `CPUQuota=200%` `MemoryMax=2G` |
| **PostgreSQL 18** | 4C | 4G | 100G | docker `limits` |
| **ClickHouse** | 4C | 8G | 500G | `max_memory_usage=6G` |
| **VictoriaMetrics** | 2C | 4G | 200G | `-memory.allowedPercent=5` |
| **Grafana** | 1C | 1G | 10G | docker `limits` |
| **vmalert + Alertmanager** | 1C | 1G | 5G | docker `limits` |
| **blackbox_exporter** | — | 128M | — | docker `limits` |
| **构建缓存（编译 nginx）** | 按需 16C | 8G | 50G | 构建容器 `cpus: 16` |
| **备份存储** | — | — | 200G | 30 天滚动 |
| **快照与产物** | — | — | 100G | 90 天滚动 |
| **小计** | **~16C（+构建 16C）** | **~28G** | **~1.2T** | |
| **业务 VM（4 个节点）** | 12C | 12G | 200G | |
| **合计** | **~28C / 112C（25%）** | **~40G / 128G（31%）** | **~1.4T / 25T（5.6%）** | |

**余量充足。** 剩余资源可用于：
- 影子/预发集群（配置演练，非常有价值）
- 压测（验证 §8.3 的容量基准）
- 构建农场（并行编译多个 nginx 版本对比）

### 14.4 关于"不是无限制使用"的执行建议

你说得对，宽裕不等于可以放任。三条硬规则：

1. **所有中间件设硬限制**（docker `limits` 或 systemd `MemoryMax`），防止 ClickHouse 一次大查询把 128G 吃光
2. **ClickHouse 的 `max_memory_usage` 必须设**（默认会用到系统内存的 90%），建议 6G
3. **日志采集有采样降载**（见 §13.4），单节点超阈值自动降采样并告警，不能让日志把磁盘写满

### 14.5 一个务实的建议：先用 Docker Compose 跑中间件

**控制面本身用 systemd 裸跑**（单二进制 Go，需要访问快照目录、签发 Agent 证书），**中间件用 Docker Compose**（PG / ClickHouse / VM / Grafana）。

理由：
- 中间件版本升级、备份、迁移全靠一个 `docker-compose.yml`，AI 好写、你好维护
- 控制面裸跑避免容器网络与宿主机文件权限的复杂问题
- 一个 `make dev` 起全套，AI 开发时迭代快

---

## 决策 15 · AI 全程编写：项目组织约定

这是为了让"AI 写代码"这件事可持续，而不只是第一轮顺、第二轮就乱。

### 15.1 核心机制：AGENTS.md 作为项目宪法

**每个 AI 会话开始，第一件事是读 `AGENTS.md`。** 它承载：

- 技术栈硬约束（不许 AI 自由替换组件）
- 目录结构与文件命名
- 代码风格与错误处理规范
- 测试要求与验收命令
- **AI 工作流规则**（任务粒度、禁止事项、自检验收）
- 常见陷阱清单

配套的 `docs/tasks/` 分里程碑任务清单，每个任务自包含（AI 新会话看不到历史对话）。

### 15.2 任务粒度原则

| 维度 | 建议值 | 理由 |
| --- | --- | --- |
| 单个任务工时 | **1–3 小时 AI 工作** | 太长会超上下文，太短失去连贯性 |
| 涉及文件数 | **3–8 个** | 超出则拆分 |
| 新增代码量 | **200–600 行** | 超出则拆分 |
| 验收方式 | **可执行命令**（`go test` / `curl` / `make xxx`） | 不许"看起来对" |

### 15.3 契约先行，实现并行

```
第一步：定义接口（Go interface + OpenAPI + proto）
        ↓ AI 生成骨架，人工确认
第二步：实现层并行开发（不同会话各做一个模块）
        ↓ 因为契约已定，不会互相打架
第三步：集成测试
```

这个平台有三条关键契约，必须先定死：
1. **控制面 ↔ Agent 的 gRPC 协议**（`proto/agent/v1/agent.proto`）
2. **内部 REST API**（`docs/api/openapi.yaml`）
3. **数据库 schema**（`ent/schema/*.go`，自动生成迁移）

### 15.4 AI 编写的四条铁律

1. **不许改架构** —— 技术栈与模块边界由 `docs/ARCHITECTURE.md` 定义，AI 只能在边界内实现
2. **不许裸写 SQL** —— 用 ent 生成的 API；确实需要复杂查询时，写在独立的 repository 文件里并加注释
3. **不许跳过错误** —— 所有 `error` 必须处理或显式 wrap（`fmt.Errorf("xxx: %w", err)`），不许 `_ =`
4. **写完必须自测** —— 每个任务有验收命令，AI 必须实际执行并贴出结果

### 15.5 给 AI 的上下文优先级

AI 上下文有限，按这个顺序喂：

```
① AGENTS.md              （必读，全局约定）
② docs/tasks/M{N}.md     （当前里程碑全部任务）
③ 涉及文件的现有内容       （让 AI 读，不要让它猜）
④ docs/ARCHITECTURE.md 的相关章节（按需，不要全给）
⑤ docs/DECISIONS.md 的相关决策（按需）
```

**不要把三份大文档一次性全塞给 AI** —— 会稀释注意力，导致它忽略关键约束。

---

## 附 D · 第二轮决策速查

| # | 决策点 | 结论 |
| --- | --- | --- |
| 8 | 百万级容量 | **2 LVS + 2 Nginx 够用，余量 1–2 个数量级**。真正要盯的是带宽（静态建议上 CDN）与"坏一台能否扛全量" |
| 9 | SQLite vs PG | **选 PostgreSQL**（PITR + JSONB + 资源管够）；**修正上轮结论** —— "单文件 cp 走天下"不成立，WAL 模式下 cp 不安全。SQLite 降级为开发态 |
| 10 | 时序数据库 | **不引入独立时序库**。指标 → VictoriaMetrics（本身就是时序库）；日志 → ClickHouse；事件/审计 → PG。存储占用 ~115GB / 25TB |
| 11 | 监控方案 | **不自研监控**。Grafana + VictoriaMetrics + Alertmanager 管"看"，自研页面管"动"。Agent 从 access log 滑窗聚合指标（不依赖 VTS 模块） |
| 12 | 编译升级 | 建**构建与升级中心**：版本兼容矩阵（patch 映射）+ 容器化可复现构建 + 9 步热升级状态机 + LVS 摘除联动 + 指令废弃检查 |
| 13 | 能力发现 | **能且必须**。三层：`nginx -V`（编译路径）→ `nginx -T`（配置树，正则切分文件边界）→ 文件系统探测（日志/轮转/分区）。角色自动识别决定可执行指令集 |
| 14 | 部署拓扑 | 控制面全家桶在物理机 A（Docker Compose），业务 VM 跨 A/B 分布，B 机做备份与影子集群。总占用 ~28C / 40G / 1.4T（余量充足） |
| 15 | AI 编程 | `AGENTS.md` 项目宪法 + `docs/tasks/` 分阶段任务清单 + 契约先行 + 四条铁律 |

### 仍需你确认（2 项）

1. **控制面部署位置**：物理机 A 上直接跑，还是单独起一台 VM？（影响 Agent 连接地址与备份策略）
2. **"百万级"口径**：日 PV 100 万，还是日请求 1000 万？（影响日志保留期与 ClickHouse 规格 —— 差 10 倍量，不过你的资源两种都扛得住）

### 建议的行动顺序

```
1. 读 AGENTS.md（项目宪法）—— 你有修改意见现在提
2. 从 docs/tasks/M0-foundation.md 开始，按 T001 → T0xx 顺序让 AI 执行
3. 每个里程碑结束跑一次集成验收（任务文件里写了命令）
4. M1 完成后（4 个节点全部上线），先手工跑一遍发布流程再继续
```

---

## 决策 16 · vSphere 虚拟化 + 万兆 LACP 环境的架构调整

> **环境更新（2026-09-03 补充）**：
> - 2 台 TH-D2110 已做**虚拟化**（VMware ESXi + vCenter），资源按需分配
> - **全万兆网络**
> - **vCenter VDS 配置了 LAG 链路聚合（LACP）**

这三条信息改变了容量结论，并引入了一批**虚拟化环境特有的坑**。本节逐条处理。

### 16.1 容量结论修订：带宽彻底不再是瓶颈

上一轮（§8.2）我把"带宽"列为唯一需要盯的指标（千兆下峰值余量只有 3 倍）。**在 2×10G LACP 环境下，这条作废。**

| 层 | 千兆环境（原评估） | **万兆 LACP（修订）** |
| --- | --- | --- |
| 聚合带宽 | 940 Mbps，余量 3 倍 | **~18 Gbps 有效，余量 ~65 倍** |
| PPS 上限 | 1.4M PPS，余量 100 倍 | **14.8M PPS，余量 1300 倍** |
| LVS Director | 余量 >100 倍 | 余量 >100 倍（不变） |
| Nginx RS | 余量 15–50 倍 | 余量 15–50 倍（不变，瓶颈在 CPU 分配） |
| **最早瓶颈** | 带宽 | **后端应用 / 数据库** |

**修订后的结论：**

> 在万兆 + LACP 环境下，**LVS 与 Nginx 这一层已经彻底退出瓶颈竞争**。2 Director + 2 RS 支撑**日 PV 千万级**（峰值约 1 万 req/s）都绰绰有余。
>
> 容量天花板现在由**后端应用与数据库**决定。平台的价值从"解决容量问题"转向"**解决变更安全问题**"——这恰恰是它原本的定位。

**唯一残留的网络约束：单流带宽上限 = 单个物理链路（10 Gbps）。**

LACP 的哈希是基于流的（五元组或 IP 对），**单条 TCP 连接只能跑在一条物理链路上**，最多 10 Gbps，无法叠加。

- 影响场景：单用户下载超大文件、单条长连接的大流量同步
- 对 nginx 常规 HTTP 服务（海量短/中连接）**完全无影响** —— 流数足够多，哈希分布均匀
- 如果真有单流 > 10Gbps 的需求，需要 25G/40G 网卡而非更多 LACP 成员

### 16.2 VMware 跑 LVS-DR + Keepalived：三个必开的端口组安全策略 ★

**这是本节最重要、也最容易踩的部分。** VMware 的虚拟交换机（VSS/VDS）端口组有三个安全策略，**默认都是「拒绝」**，而 LVS-DR 与 Keepalived 会同时触发其中两个（有时三个）。

#### 三个策略分别管什么

| 策略 | 含义 | 默认值 |
| --- | --- | --- |
| **Promiscuous Mode（混杂模式）** | vSwitch 是否把**目标 MAC 不是该 vNIC** 的帧也交给它 | Reject |
| **MAC Address Changes（MAC 地址更改）** | Guest OS **主动改** vNIC 的 MAC 后，是否允许该 MAC 的帧通过 | Reject |
| **Forged Transmits（伪传输）** | Guest 发出的帧，**源 MAC 与 vNIC 分配的不一致**时是否放行 | Reject |

#### 逐个分析为什么必须开

**① Keepalived VRRP → 需要「MAC 地址更改」+「混杂模式」**

VRRP 使用虚拟 MAC：`00:00:5E:00:01:<VRID>`（IPv4）。Master 节点会用这个虚拟 MAC 响应 VIP 的 ARP 请求，并接收目标 MAC 为虚拟 MAC 的帧。

- Guest 用了一个**不是 vSwitch 分配**的 MAC 作为源发帧 → 触发「MAC 地址更改」→ **默认被拒绝**
- vSwitch 需要把目标 MAC = 虚拟 MAC 的帧交给该 vNIC → 需要「混杂模式」

**症状（如果没开）**：VRRP 通告发不出去 / 收不到 → **双机都认为自己是 Master → 脑裂**，或者主备不切换。

**② LVS-DR 转发 → 需要「伪传输」**

DR 模式的工作原理：Director 收到包后，**只改目标 MAC**（改成选中的 RS 的 MAC），源 MAC 保持为上游路由器/客户端的 MAC，然后从 vNIC 发出。

```
入站： [src MAC = 路由器] [dst MAC = VIP 的虚拟 MAC] [dst IP = VIP]
                    ↓ LVS-DR 改目标 MAC
出站： [src MAC = 路由器] [dst MAC = RS 的 MAC]       [dst IP = VIP]
        ↑ 源 MAC 不是 Director vNIC 的 MAC → Forged Transmit！
```

**症状（如果没开）**：vSwitch 静默丢弃这些帧 → **RS 收不到任何请求**，但 `ipvsadm -Ln` 显示转发计数在涨（因为计数发生在内核 IPVS 层，包还没到网卡就被 vSwitch 丢了）。这是**最坑的地方——表象与真实原因完全脱节**，排查时极易误判为 RS 的 ARP 配置问题。

**③ 混杂模式要不要开？**

- **Director**：VMware 官方 KB 建议 VRRP 场景开启。虽然严格分析下 LVS-DR 的入站帧目标是虚拟 MAC、出站是伪传输，理论上混杂不是必需，但**实测中不开混杂常出现 VRRP 收不到对端通告**的诡异现象。**建议开**。
- **RS**：**不需要开**。RS 收到的是目标 MAC = 自己 MAC 的普通帧。开了反而有性能与安全代价。

#### 推荐配置（隔离端口组，不要全局开）

```
vCenter → 网络 → 选择 VDS → 分布式端口组

┌─ DPortGroup-LVS-Director ────────────────────┐
│  安全策略：                                      │
│    混杂模式         → 接受   ✓                  │
│    MAC 地址更改     → 接受   ✓                  │
│    伪传输           → 接受   ✓                  │
│  成组和故障切换：                                 │
│    负载均衡         → 基于 IP 哈希（见 §16.3）    │
└──────────────────────────────────────────────┘

┌─ DPortGroup-Nginx-RS ────────────────────────┐
│  安全策略：                                      │
│    混杂模式         → 拒绝   （保持默认）         │
│    MAC 地址更改     → 接受   ✓（稳妥起见）       │
│    伪传输           → 接受   ✓（稳妥起见）       │
└──────────────────────────────────────────────┘
```

**关键建议：Director 单独一个端口组（独立 VLAN 更好），只在这个端口组开混杂模式。**

理由：
- 混杂模式会让该 VM 看到同广播域内**所有**流量 —— 既是信息泄露风险，也有 CPU 开销（要处理大量无关帧）
- 把 Director 隔离到独立端口组，把爆炸半径限制到最小
- RS 端口组保持默认，业务 VM 不受影响

> **AI 实现提示**：这些策略无法通过 Guest OS 内配置绕过，**必须在 vCenter 侧配置**。平台的"DR 合规巡检"（决策 4）无法通过 Agent 检测这一项 —— 因为它发生在 hypervisor 层。所以要在部署文档里做成**安装清单的强制项**，并在平台里提供一个"部署前检查清单"页面。

#### 排查对照表（症状 → 原因）

| 症状 | 可能原因 |
| --- | --- |
| `ipvsadm` 转发计数增长，但 RS 访问日志无请求 | **Director 端口组「伪传输」= 拒绝** ★ 最典型 |
| 双机都是 Master / VIP 同时出现在两台 | 「MAC 地址更改」= 拒绝，或「混杂模式」= 拒绝 |
| 主备切换正常，但 VIP 不通 | 「伪传输」= 拒绝 |
| 时通时断，重启后恢复 | ARP 抑制（arp_ignore）漂移，或 LACP 哈希不均 |
| `arping VIP` 能通但业务不通 | DR 转发被丢，检查「伪传输」 |

### 16.3 VDS LAG / LACP：四个必须注意的点

**① LAG 成员链路必须终结在同一台物理交换机（或支持 MLAG/堆叠的交换机对）**

标准 LACP 要求 LAG 的所有成员属于同一个逻辑交换机。如果两条上行分别连到两台**独立**（未堆叠、未做 MLAG）的物理交换机，LACP 协商会异常。

- 你的环境要确认：2 台 TH-D2110 的万兆上行，接到的是**同一台交换机**，还是两台做了堆叠/vPC/MLAG 的交换机？
- 如果是两台独立交换机，**不能做跨设备 LACP** —— 应改为：每台主机各自做一个 LAG 到本地交换机，或者改用 **LBT（基于物理网卡负载的成组）**，后者不需要交换机侧配合

**② 负载均衡算法建议用「基于 IP 哈希」**

| 算法 | 说明 | 建议 |
| --- | --- | --- |
| 基于源虚拟端口（默认） | 按 vNIC 分配，VM 的所有流量走一条链路 | ❌ 对 nginx 这种单 VM 高流量场景，只会用一条 10G |
| **基于 IP 哈希** | 按源/目标 IP 对哈希，不同流分散到不同链路 | ✅ **推荐** |
| 基于源 MAC 哈希 | 粒度太粗 | ❌ |
| 基于物理网卡负载（LBT） | 动态迁移，不需交换机配合 | ✅ 如果交换机不支持 LACP 时的替代方案 |

**选「基于 IP 哈希」的理由**：
- 同一对 IP 的流量固定走同一条链路 → **不会乱序**（TCP 乱序会触发重传，性能反而下降）
- 不同流分散 → 真正利用多链路带宽

**③ 单流上限 10 Gbps（前面说过，这里重申）**

大文件下载、单条长连接同步场景要注意。常规 HTTP 服务无影响。

**④ LACP 与 LVS-DR 的交互**

DR 只改目标 MAC，**不改 IP**。所以 LACP 的 IP 哈希在 Director 出站时看到的仍是原始 IP 对 → 出站链路与入站链路一致（哈希输入相同），**不会造成额外乱序**。这个组合是安全的。

### 16.4 vMotion / DRS：反亲和性与中断风险

**风险 1：Director 主备被 vMotion 到同一台物理主机**

两台 ESXi，如果 DRS 把 director-01 和 director-02 都调度到主机 A，那么主机 A 宕机 = 整个 LVS 层全灭（尽管概率低）。

**对策：配置 DRS 反亲和性规则**

```
vCenter → 集群 → 配置 → DRS 规则 → 添加
  类型：虚拟机到虚拟机反亲和性（Separate Virtual Machines）
  成员：director-01, director-02
```

同理 `rs-nginx-01` 与 `rs-nginx-02` 也应配置反亲和性 —— **保证单台物理机宕机时，业务层仍能承载全量**（呼应 §8.3 的 N+1 原则）。

**风险 2：Director 发生 vMotion 时的 VIP 中断**

vMotion 迁移 Director VM 时：
- VIP 与虚拟 MAC 会随 VM 迁移
- 迁移瞬间上游路由器/交换机的 MAC 表仍指向旧物理口 → **短暂丢包**
- 迁移完成后 VM 发 GARP（无故 ARP）刷新 MAC 表 → 恢复
- 典型中断时间：**< 1 秒**（通常几百毫秒）

**对策**：
1. 对 Director VM 设置 **DRS 自动化级别 = 手动（Manual）** 或 **部分自动化**，避免 DRS 因负载均衡随意 vMotion 关键节点
2. 保留自动 vMotion 仅在主机故障/维护模式时触发（这是 HA 需要的）
3. 平台的脑裂检测（两台同时上报持 VIP）要设置**容忍窗口 > 3 秒**，避免 vMotion 期间误报

**风险 3：RS 的 vMotion**

影响较小（RS 无状态，且 LVS 健康检查会摘除失联节点）。但要注意 vMotion 期间 LVS 可能把 RS 判定为不可用而摘除 —— 平台的 `check` 参数（rise/fall）要设置得能容忍几秒抖动：

```nginx
check interval=3000 rise=2 fall=3 timeout=1000;
#     间隔3秒        连续2次成功才加回 / 连续3次失败才摘除 → 摘除需要 9 秒，能容忍 vMotion
```

### 16.5 Guest 侧性能调优（万兆必做）

| 项 | 配置 | 理由 |
| --- | --- | --- |
| **网卡类型** | **vmxnet3**（绝不用 e1000/e1000e） | e1000 在万兆下 CPU 占用极高，且不支持多队列 |
| **多队列（RSS）** | 队列数 = vCPU 数（如 8） | 把中断分散到多个 vCPU，单核不会成为瓶颈 |
| **TSO / GRO** | RS 上**开启**，Director 上观察 | 减少大流量下 CPU 开销（分段卸载到网卡） |
| **LRO** | Director 上建议**关闭** | LRO 会合并 TCP 段，可能干扰 LVS 的包处理 |
| **vNUMA** | VM vCPU > 8 时启用 | TH-D2110 是双路（2×28 核），有 NUMA 拓扑 |
| **CPU 热添加** | 关闭 | 开启会禁用 vNUMA，影响 NUMA 感知性能 |

**RSS 队列配置**（Guest 内）：

```bash
# 查看当前队列数
ethtool -l eth0
# 设置合并队列数为 8（与 vCPU 数一致）
ethtool -L eth0 combined 8
# 中断亲和性（可选，让每个队列绑一个 CPU）
# 现代内核 irqbalance 会自动处理，一般不用手工配
```

**vCPU 数量与 NUMA**：

TH-D2110 是 Xeon Gold 6330（28 核 56 线程），双路 = 56 核 112 线程。**单个 NUMA 节点 = 28 核 56 线程**。

- VM 的 vCPU 数 ≤ 56 时可放进单个 NUMA 节点，避免跨节点内存访问
- nginx VM 给 8 vCPU，远小于 56 → **天然 fit 在单 NUMA 节点内，无需特别处理**
- 只有给 VM 分配 > 56 vCPU 时才需要考虑 vNUMA

### 16.6 时间同步：虚拟化环境的高优先级项

**这个问题在虚拟化环境比物理机严重得多**，而时间不准会让整个日志检索体系失效。

**问题**：VM 的时钟由 hypervisor 虚拟，负载高、vMotion、快照恢复都会导致**时钟漂移**。跨节点日志检索依赖时间对齐，偏差几秒就会导致"按时间范围查不到关联请求"。

**对策**：

```bash
# 1. 所有 VM 安装 chrony，配置相同 NTP 源
dnf install -y chrony
cat > /etc/chrony.conf <<'EOF'
server ntp.aliyun.com iburst
server time.cloudflare.com iburst
makestep 1.0 3
rtcsync
EOF
systemctl enable --now chronyd

# 2. ★ 关闭 VMware Tools 的周期性时间同步，避免与 NTP 打架
vmware-toolbox-cmd timesync disable
# 或在 VMX 配置里设置（永久）：
#   tools.syncTime = "FALSE"
#   time.synchronize.continue = "FALSE"
#   time.synchronize.restore = "FALSE"
```

**为什么必须关 VMware Tools 时间同步**：两者会互相"纠正"，导致时钟反复跳变。NTP 是渐进调整（slew），VMware Tools 是粗暴跳变（step），跳变会破坏日志时序。**用 NTP，不用 VMware Tools 同步。**

**平台侧**：Agent 上报节点的 NTP 同步状态与时间偏差，**偏差 > 1 秒即告警**（这是日志可信度的地基）。

### 16.7 VM 规格分配表（按需分配，但关键 VM 要预留）

| VM | 角色 | vCPU | 内存 | 磁盘 | 建议主机 | 预留策略 |
| --- | --- | --- | --- | --- | --- | --- |
| **ngxcp-ctrl** | 控制面全家桶（PG / ClickHouse / VM / Grafana / 控制面） | **8** | **32G** | **2T** | A | CPU 4G 预留 |
| **director-01** | Keepalived MASTER + ipvs | 2 | 4G | 40G | A | **全部预留** |
| **director-02** | Keepalived BACKUP + ipvs | 2 | 4G | 40G | **B**（反亲和） | **全部预留** |
| **rs-nginx-01** | Nginx RealServer | **8** | **16G** | 200G | A | CPU 4G 预留 |
| **rs-nginx-02** | Nginx RealServer | **8** | **16G** | 200G | **B**（反亲和） | CPU 4G 预留 |
| **ngxcp-bak**（可选） | 备份接收 + 影子集群 | 8 | 16G | 2T | B | 无预留 |
| **小计（主机 A）** | | 18 vCPU | 52G | — | | |
| **小计（主机 B）** | | 18 vCPU | 36G | — | | |
| **单机余量** | | **94 线程** | **76G** | | | |

两台主机合计可用：224 线程 / 256G / 50T。**当前占用不到 20%，余量充足**，可随时加 RS 节点或建预发环境。

**为什么 Director 要"全部预留"**：
- "按需分配"靠 shares 竞争，资源紧张时关键节点会被挤压
- Director 是流量入口，2 vCPU / 4G 的绝对量很小，**全部预留的成本极低，收益（确定性）极高**
- nginx RS 给 8 vCPU / 16G，预留一半（4 vCPU / 8G）作为保底

**为什么 RS 给 8 vCPU**：
- nginx 事件驱动，worker 数 = vCPU 数，8 worker 配合 vmxnet3 RSS 8 队列
- 这个配置跑满万兆（数十万 QPS 级静态、数万 QPS 级反代）没问题
- 实测后如果富余太多可以降到 4，但**先给足再优化**，避免一开始就卡 CPU

### 16.8 部署形态调整：控制面建议全面容器化

上一轮（§14.5）我建议"控制面 systemd 裸跑 + 中间件 Docker Compose"。**在 vSphere 环境里，这个建议改为：全部容器化。**

理由：
- 控制面在一个独立 VM（ngxcp-ctrl）里，容器化后**整个平台的备份 = 备份一组 volume 目录**
- 迁移/重建 = `docker compose up` 一条命令（在 B 机上起影子环境也是）
- AI 写 compose 比写 systemd unit + 文件权限更不容易出错

```yaml
# docker-compose.yml（ngxcp-ctrl 上）
services:
  ngxcp-server:
    image: ngxcp/server:latest
    build: { context: ., dockerfile: docker/server.Dockerfile }
    ports: ["8080:8080", "9443:9443"]      # 9443 = Agent gRPC (mTLS)
    volumes:
      - ./config.yaml:/etc/ngxcp/config.yaml:ro
      - pki:/etc/ngxcp/pki                  # Agent CA（备份必含！）
      - snapshots:/var/lib/ngxcp/snapshots  # 发布前快照
      - artifacts:/var/lib/ngxcp/artifacts  # 构建产物
    depends_on: [postgres, clickhouse, victoriametrics]
    deploy: { resources: { limits: { cpus: '2', memory: 2G } } }

  postgres:
    image: postgres:18-alpine
    volumes: [pgdata:/var/lib/postgresql/data, ./wal_archive:/wal_archive]
    command: postgres -c wal_level=replica -c archive_mode=on \
                      -c archive_command='test ! -f /wal_archive/%f && cp %p /wal_archive/%f' \
                      -c shared_buffers=2GB -c log_min_duration_statement=500
    deploy: { resources: { limits: { cpus: '4', memory: 4G } } }

  clickhouse:
    image: clickhouse/clickhouse-server:24
    ulimits: { nofile: { soft: 262144, hard: 262144 } }
    environment:
      CLICKHOUSE_SKIP_USER_SETUP: 1
    volumes:
      - chdata:/var/lib/clickhouse
      - ./configs/clickhouse/max_memory.xml:/etc/clickhouse-server/config.d/max_memory.xml:ro
    deploy: { resources: { limits: { cpus: '4', memory: 8G } } }

  victoriametrics:
    image: victoriametrics/victoria-metrics:v1.102.0
    command:
      - --storageDataPath=/victoria-metrics-data
      - --retentionPeriod=1y
      - --memory.allowedPercent=5
    volumes: [vmdata:/victoria-metrics-data]
    ports: ["8428:8428"]
    deploy: { resources: { limits: { cpus: '2', memory: 4G } } }

  grafana:
    image: grafana/grafana:11.4.0
    environment:
      GF_AUTH_PROXY_ENABLED: "true"
      GF_AUTH_PROXY_HEADER_NAME: "X-WEBAUTH-USER"
      GF_AUTH_PROXY_WHITELIST: "127.0.0.1"
      GF_AUTH_PROXY_AUTO_SIGN_UP: "true"
    volumes:
      - grafanadata:/var/lib/grafana
      - ./configs/grafana/provisioning:/etc/grafana/provisioning:ro   # 面板 JSON 纳管
    deploy: { resources: { limits: { cpus: '1', memory: 1G } } }

  vmalert:
    image: victoriametrics/vmalert:v1.102.0
    command:
      - --datasource.url=http://victoriametrics:8428
      - --notifier.url=http://alertmanager:9093
      - --rule=/etc/vmalert/*.yml
    volumes: [./configs/vmalert:/etc/vmalert:ro]

  alertmanager:
    image: prom/alertmanager:v0.27.0
    volumes: [./configs/alertmanager:/etc/alertmanager:ro]
    command:
      - --webhook.url=http://ngxcp-server:8080/api/v1/alerts/ingest   # 告警汇总到平台

volumes: { pki: {}, snapshots: {}, artifacts: {}, pgdata: {}, chdata: {}, vmdata: {}, grafanadata: {} }
```

**备份范围（必含）**：
```
docker compose 的 volume 目录（pgdata / chdata / vmdata / pki / snapshots / artifacts / grafanadata）
+ pg_dump 逻辑备份（跨版本可恢复）
+ config.yaml 与 configs/ 目录（含 module_matrix.json、Grafana 面板 JSON）
★ pki/ 丢失 = 所有 Agent 需重新注册，这是最容易被忽略的灾难场景
```

### 16.9 本轮环境变更带来的新风险

| 风险 | 影响 | 对策 |
| --- | --- | --- |
| **端口组安全策略未配置** | LVS-DR 完全不通，且表象误导（ipvsadm 计数正常但 RS 无请求） | **做成部署检查清单的强制项**；平台提供部署前自检页 |
| **Director 与 RS 被调度到同一物理主机** | 单机故障导致业务全灭 | DRS 反亲和性规则（Director 一对、RS 一对） |
| **VM 时钟漂移** | 跨节点日志检索错位 | chrony + 关闭 VMware Tools 时间同步 + Agent 上报偏差告警 |
| **LAG 成员跨独立交换机** | LACP 协商异常、流量黑洞 | 确认物理交换机是否堆叠/MLAG；否则改 LBT |
| **LACP 算法为默认（源虚拟端口）** | 万兆只用到一条 10G | 改为「基于 IP 哈希」 |
| **e1000 网卡** | 万兆下 CPU 打满 | 强制 vmxnet3 + RSS 多队列 |
| **资源按需分配导致关键 VM 被挤压** | 高负载时 Director 丢包 | Director 全部预留，RS 预留 50% |
| **vMotion 触发误摘除/误脑裂告警** | 告警噪音、不必要的摘除 | check 参数调优（fall=3）+ 脑裂检测容忍窗口 > 3s |

### 16.10 给运维的 VMware 部署检查清单（做成平台页面）

平台里做一个「**部署前检查清单**」页面，逐项勾选，未通过的不允许纳管节点：

**vCenter 侧（无法通过 Agent 检测，必须人工确认）：**
- [ ] Director 端口组：混杂模式 = 接受
- [ ] Director 端口组：MAC 地址更改 = 接受
- [ ] Director 端口组：伪传输 = 接受 ★
- [ ] RS 端口组：MAC 地址更改 / 伪传输 = 接受
- [ ] Director 与 RS 使用独立端口组（混杂模式不扩散）
- [ ] VDS LAG 负载均衡算法 = 基于 IP 哈希
- [ ] LAG 成员链路终结于同一物理交换机（或交换机已堆叠/MLAG）
- [ ] DRS 反亲和性规则：director-01 ⊥ director-02
- [ ] DRS 反亲和性规则：rs-nginx-01 ⊥ rs-nginx-02
- [ ] Director VM 的 DRS 自动化级别 = 手动（避免随意 vMotion）

**Guest 侧（Agent 可自动检测）：**
- [ ] 网卡类型 = vmxnet3
- [ ] RSS 队列数 = vCPU 数
- [ ] chrony 运行且已同步，偏差 < 1s
- [ ] VMware Tools 时间同步已关闭
- [ ] VIP 绑定在 lo，掩码 /32
- [ ] arp_ignore=1 / arp_announce=2（all 与 lo）
- [ ] rp_filter=0（all / default / 物理网卡）
- [ ] VIP 不在物理网卡上
- [ ] LVS 虚拟服务端口 == RS 端口
- [ ] Director 与 RS 同二层（arping 探测）
- [ ] `/etc/nginx` 与 staging 目录同分区（原子 rename 前提）

---

## 附 E · 第三轮决策速查（环境更新）

| # | 决策点 | 结论 |
| --- | --- | --- |
| 16.1 | 万兆 LACP 下的容量 | **带宽彻底不是瓶颈**（余量 ~65 倍）。2+2 支撑日 PV 千万级都够。瓶颈转移到后端应用/DB |
| 16.2 | VMware 端口组安全策略 | **Director 端口组必须开「混杂模式」+「MAC 地址更改」+「伪传输」**；漏「伪传输」= DR 转发被静默丢弃，且表象极具误导性 |
| 16.3 | LACP 配置 | 负载均衡算法改**基于 IP 哈希**（默认"源虚拟端口"只会用一条 10G）；确认链路终结于同一交换机；单流上限 10G |
| 16.4 | vMotion / DRS | 配置**反亲和性规则**（Director 一对、RS 一对）；Director DRS 自动化设为手动；脑裂检测容忍窗口 > 3s |
| 16.5 | Guest 调优 | **vmxnet3 + RSS 多队列**（队列数 = vCPU）；RS 开 TSO/GRO，Director 关 LRO |
| 16.6 | 时间同步 | chrony + **关闭 VMware Tools 时间同步**（两者打架会跳变，破坏日志时序）；偏差 > 1s 告警 |
| 16.7 | VM 规格 | ctrl 8C/32G、director 2C/4G（**全预留**）、rs 8C/16G（预留 50%）。单机占用 < 20% |
| 16.8 | 部署形态 | 修订为**全面容器化**（一个 compose 管全套），备份 = 备份 volume + pg_dump，**pki/ 必含** |

### 新增待确认（2 项）

1. **物理交换机是否堆叠 / 支持 MLAG？** —— 决定了 LACP 是否可用（跨独立交换机做不了 LACP），不可用则改 LBT
2. **vCenter / ESXi 版本？** —— VDS 的增强型 LACP 需要 vSphere 5.5+；部分特性（如 LAG 的 IP 哈希）在不同版本行为有差异
