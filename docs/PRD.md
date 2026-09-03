# Nginx 集群管理平台 · 产品需求文档（PRD）

| 项    | 内容                              |
| ---- | ------------------------------- |
| 产品代号 | **NGX-CP**（Nginx Control Plane） |
| 版本   | v0.1（方案阶段）                      |
| 日期   | 2026-09-03                      |
| 目标读者 | 运维架构师 / SRE / 平台工程团队            |
| 文档状态 | 待评审                             |

---

## 0. 一页纸结论（TL;DR）

> **本平台的核心不是"批量改配置文件"，而是把 Nginx 变更变成一条"可校验、可灰度、可观测、可一键回滚"的流水线。**

三句话产品定义：

1. **一处改，全局达** —— 配置以模板 + 变量方式集中定义，节点只消费渲染结果，杜绝配置漂移。
2. **先验证，再下发** —— 任何配置落地前必须经过 `nginx -t` 语法校验 + 语义规则检查 + 灰度批次验证，语法错误永远打不到生产。
3. **随时能退回去** —— 每一次变更自动留下配置快照 + Git 提交，回滚 = 选一个历史版本点一下。

**与 LVS+DR 的关系**：LVS 调度层（VIP / RealServer / 调度算法 / 健康检查）与 Nginx 七层层（upstream / server / location / 证书）在平台内是**两套模型、一个控制台**，共享同一份节点池和同一条发布流水线。这是本平台区别于"又一个配置分发工具"的关键差异点。

---

## 1. 背景与痛点

### 1.1 现状问题（按严重度排序）

| #  | 痛点                            | 后果                                       | 现状做法                   |
| -- | ----------------------------- | ---------------------------------------- | ---------------------- |
| P0 | 配置在多节点间手工同步                   | 配置漂移，节点行为不一致，排障时"这台上明明是好的"               | `scp` + `for` 循环       |
| P0 | 语法错误直接上生产                     | `nginx -t` 忘跑或只在本地跑 → reload 失败甚至 502 雪崩 | 人肉纪律                   |
| P0 | 无快速回滚手段                       | 故障期间翻备份目录找文件，MTTR 以分钟~小时计                | 靠记忆 + 备份脚本             |
| P1 | 证书分散在各机器                      | 漏续期导致业务 HTTPS 中断，事故往往由外部用户先发现            | crontab + certbot 各跑各的 |
| P1 | LVS/Keepalived 配置与 Nginx 配置割裂 | 摘除 RS 后忘改 LVS，或改了 LVS 没同步 Nginx upstream | 两套文档两套人                |
| P1 | 变更无审计                         | 出事后无法回答"谁、什么时候、改了什么、影响哪些节点"              | 翻 shell history        |
| P2 | 节点无统一健康视图                     | 节点存活、Nginx 版本、连接数、错误率靠逐个登录看              | 多标签页 SSH               |
| P2 | 无并发变更保护                       | 两个人同时改同一个集群，后提交的覆盖前者                     | 口头协调                   |

### 1.2 目标用户

| 角色             | 主要诉求              | 高频场景                   |
| -------------- | ----------------- | ---------------------- |
| 运维 / SRE（主力）   | 批量、可靠、可回滚地变更      | 加域名、改 upstream、发证书、摘节点 |
| 平台工程           | 标准化与自助化           | 给业务方开"改自己站点配置"的权限      |
| 业务研发（只读/受限写）   | 自助改自己负责的 server 块 | 改 location 路由、加 header |
| 团队 Leader / 安全 | 审计与合规             | 查看变更记录、审批高危操作          |

---

## 2. 产品定位与边界

### 2.1 做什么

- 多节点 Nginx（含 OpenResty / Tengine）的统一纳管与可视化
- 配置的**版本化编辑 → 校验 → 灰度发布 → 观测 → 回滚**闭环
- SSL/TLS 证书的**集中签发（ACME）→ 存储 → 分发 → 到期告警**全生命周期
- LVS/DR 模式下 **VIP、RealServer、调度策略、健康检查**的编排与下发
- 配置/证书备份、差异对比、一键恢复
- 全量操作审计

### 2.2 不做什么（明确划清，避免范围蔓延）

| 不做                                | 原因 / 替代方案                                       |
| --------------------------------- | ----------------------------------------------- |
| 不替代 Kubernetes Ingress Controller | 容器场景交给 Ingress；本平台面向**裸机/VM 上的 Nginx 集群**       |
| 不做通用 CMDB / 主机管理                  | 只维护与 Nginx/LVS 相关的节点最小信息集                       |
| 不做 APM / 全链路追踪                    | 只提供节点级健康指标与 Nginx stub_status / access log 关键指标 |
| 不做 WAF 规则引擎                       | 只做 WAF 配置的**下发与版本管理**，规则内容由人写                   |
| 不接管业务进程                           | 只管 Nginx/LVS 自身进程（start/stop/reload/test）       |

---


## 3. 信息架构（IA）

```
NGX-CP 控制台
├── 概览 Dashboard                  # 全局健康、待办、变更动态
├── 集群与节点 Cluster
│   ├── 集群/分组管理 Group          # 按机房/环境/业务线划分
│   ├── 节点管理 Node                # 纳管、Agent 状态、版本、指标
│   ├── 能力基线 Capability         # nginx -V 解析：版本/prefix/模块清单 + 双机一致性
│   └── 节点注册/令牌 Enroll         # 一行命令接入
├── 配置中心 Config
│   ├── 配置文件树                    # nginx.conf / conf.d/ / stream.d/
│   ├── 模板与变量 Template/Variable  # Jinja2 模板 + 三级变量
│   ├── 渲染预览 & Diff              # 所见即所得 + 版本对比
│   └── 语法/语义校验 Check           # nginx -t + 规则引擎
├── 发布任务 Deploy                   # 变更流水线核心页
│   ├── 新建发布（向
导）              # 选范围 → 校验 → 灰度策略 → 确认
│   ├── 任务详情（实时进度/批次）      # 逐节点状态、可暂停/中止/回滚
│   └── 历史任务                      # 列表 + 筛选
├── 证书管理 Cert
│   ├── 证书列表（到期色阶/告警）
│   ├── ACME 申请与自动续期          # DNS-01 / HTTP-01
│   ├── 手动上传（自签/商业证书）
│   └── 证书分发与绑定               # 证书 ↔ 域名 ↔ 集群
├── LVS 管理 LVS                      # 仅 LVS+DR 架构启用
│   ├── Director（调度器）管理
│   ├── VIP / 虚拟服务 VS
│   ├── RealServer（RS = Nginx 节点）
│   └── Keepalived（VIP 高可用/漂移）
├── 备份与恢复 Backup
│   ├── 自动快照（变更前后）
│   ├── 手动快照
│   └── 恢复/回滚（选版本 → 选范围 → 执行）
├── 日志与安全 Security                # 新增，见 §4.4
│   ├── 统一日志检索                     # 跨节点聚合、TraceID 追踪、聚合视图
│   ├── 攻击检测规则                     # 注入/扫描器/CC/爆破/5xx 突增
│   ├── 告警中心                        # 事件流、分级、处置状态
│   └── 封禁变更单                      # 复用发布流水线，可回滚
└── 系统 System
    ├── 审计日志 Audit
    ├── 用户与权限 RBAC
    ├── 通知渠道（钉钉/飞书/企微/Webhook）
    └── 平台设置（校验规则、并发限制、Agent 版本）
```

---

## 4. 功能需求清单（MoSCoW 优先级）


### 4.1 P0 — MVP 必须有（v0.1，约 6 周）

| 模块 | 功能点        | 说明                                                      |
| -- | ---------- | ------------------------------------------------------- |
| 节点 | 节点纳管       | Agent 注册令牌 + 一行命令接入；支持按集群/分组打标签                         |
| 节点 | 心跳与状态      | 5s 心跳，30s 未上报标记 `Offline`；展示 Nginx 版本、进程状态、配置最后更新时间     |
| 节点 | 远程操作       | 单机/批量 `nginx -t` / `reload` / `restart` / `stop`（含二次确认） |
| 配置 | 配置文件浏览     | 读取节点真实配置，按 `nginx.conf` / `conf.d/*.conf` 树形展示          |
| 配置 | 在线编辑       | Monaco 编辑器，Nginx 语法高亮；保存即生成新版本（不直接下发）                   |
| 配置 | 版本管理       | 每次保存生成一个 commit；支持版本列表、Diff（并排）、回滚到任意版本                 |
| 配置 | 语法校验       | 在 Agent 本地以临时文件执行 `nginx -t -c`，返回原始 stderr             |
| 配置 | 语义校验       | 内置规则：upstream 引用存在、根路径存在、证书路径存在、端口冲突、重复 server_name     |
| 配置 | 模板与变量      | Jinja2 模板 + 三级变量（全局/集群/节点），支持渲染预览                       |
| 发布 | 灰度发布       | 批次策略：按比例（10%/50%/100%）或按节点显式指定；批次间可暂停                   |
| 发布 | 发布原子步骤     | 备份 → 下发 → `-t` 校验 → reload → 探活 → 成功/自动回滚               |
| 发布 | 自动回滚       | 任一批次内失败节点数超阈值（默认 0）立即中止并对已下发节点回滚                        |
| 发布 | 配置漂移检测     | 定时对节点配置做哈希比对，与期望版本不一致则告警并可一键修复                          |
| 证书 | 证书清单       | 列出所有纳管证书：域名、签发者、到期时间、绑定集群；<30 天黄色、<7 天红色                |
| 证书 | 手动上传       | 上传 fullchain.pem + privkey.pem，加密存储                     |
| 证书 | 分发与 reload | 证书绑定到集群 → 下发到 `/etc/nginx/ssl/<domain>/` → reload       |
| 备份 | 自动快照       | 每次发布前自动打包 `/etc/nginx/`（含 conf.d、ssl）上传归档               |
| 备份 | 一键恢复       | 选快照 → 选节点范围 → 走标准发布流水线恢复                                |
| 系统 | 审计日志       | 记录：谁、何时、对什么、做了什么、结果、来源 IP；不可删除                          |
| 系统 | RBAC       | 角色：管理员 / 运维 / 只读；动作级权限；发布高危操作需二次确认                      |
| 系统 | 登录认证       | 本地账号 + OIDC/LDAP 预留；全站 HTTPS，敏感字段加密                     |

### 4.2 P1 — 重要（v0.2）

| 模块  | 功能点           | 说明                                                                |
| --- | ------------- | ----------------------------------------------------------------- |
| 证书  | ACME 自动签发与续期  | 支持 Let's Encrypt / ZeroSSL；DNS-01（阿里云/腾讯/CF）+ HTTP-01；到期前 20 天自动续 |
| LVS | Director 纳管   | 识别 `ipvsadm` / `keepalived` 版本，读取当前规则并反向建模                        |
| LVS | VIP / 虚拟服务    | 增删改 VS：VIP:Port、协议 TCP/UDP、调度算法（rr/wrr/lc/wlc/sh/dh）、持久超时         |
| LVS | RealServer 管理 | RS 从节点池选择；权重、端口映射；**一键摘除/上线（权重=0）**                               |
| LVS | Keepalived 配置 | 主备角色、优先级、VRRP 实例、健康检查脚本、通知脚本                                      |
| LVS | 拓扑可视化         | VIP → Director → RS 的链路图，实时显示 RS 健康与权重                            |
| 发布  | 定时发布          | 指定未来时间点执行（如低峰期 02:00）                                             |
| 发布  | 发布审批流         | 高危集群（生产标记）变更需 Leader 审批后执行                                        |
| 监控  | 节点指标          | 采集 stub_status（Active/Reading/Writing/Waiting）+ 系统负载 + 磁盘         |
| 监控  | 发布期观测         | 发布批次间展示目标节点的 QPS、5xx 率、响应时间曲线，异常自动中止                              |
| 通知  | 多渠道告警         | 钉钉/飞书/企微/Webhook；发布结果、证书到期、节点离线、漂移                                |
| 日志  | 统一日志格式       | 平台下发标准 JSON `log_format`（含 `request_id` / `upstream_addr` / 各段耗时），一键应用 |
| 日志  | TraceID 透传      | `X-Request-Id` 回写客户端 + 透传后端；支持按 ID 检索全链路                          |
| 日志  | 跨节点检索         | 时间/节点/状态码/URI/IP/request_id/耗时 多维筛选；正则支持；原文展开                    |
| 日志  | 聚合分析视图       | Top URI / Top IP / Top UA / 状态码分布 / P50·P95·P99 耗时曲线                 |
| 日志  | Agent 内置采集     | tail + offset 持久化 + 本地磁盘队列（断连补传）+ 采样降载                          |
| 安全  | 攻击检测规则引擎    | 滑动窗口统计：注入特征、扫描器指纹、目录爆破、CC 洪水、敏感路径、Slowloris、5xx/4xx 突增 |
| 安全  | 告警中心           | 事件流 + 分级（INFO/WARN/CRITICAL）+ 处置状态 + 证据样本                        |
| 安全  | 封禁变更单         | 命中规则 → 生成 `deny` 片段 → **复用发布流水线**（校验/灰度/探活/回滚）→ 全程审计     |
| 安全  | 分级处置策略       | 自动（高置信直接执行）/ 半自动（等审批）/ 仅告警（手动处置），按规则可配               |

### 4.3 P2 — 后续（v0.3+）

| 模块  | 功能点                                                          |
| --- | ------------------------------------------------------------ |
| 配置  | 配置片段库（Snippets）：常用 location / 反代 / 缓存 模板复用                   |
| 配置  | AI 辅助审查：提交前自动指出高风险配置（如 `proxy_pass` 尾部斜杠陷阱）                  |
| 证书  | 证书私钥托管加密（KMS/Vault）+ 私钥不出平台                                  |
| LVS | LVS 连接数/流量统计可视化（ipvsadm 统计数据）                                |
| 平台  | 多租户：按业务线隔离集群与权限                                              |
| 平台  | OpenAPI + CLI 工具，接入 CI/CD（`ngxcp deploy --cluster prod-web`） |
| 平台  | 灾备演练：一键模拟节点故障，验证摘除与回滚链路                                      |

---

## 5. 核心业务流程

### 5.1 流程 A：配置变更（主流程）

```
[选择范围] → [编辑配置] → [保存新版本] → [校验] → [选择灰度策略] → [执行发布]
                                              │                          │
                                              └─ 失败：返回编辑           ├─ 批次1：备份→下发→nginx -t→reload→探活
                                                                          ├─ 观测窗口（可配置 30s~5min）
                                                                          ├─ 批次2 … 批次N
                                                                          └─ 失败超阈值：中止 + 已下发节点自动回滚
```

**关键规则**：

1. 编辑保存 ≠ 生效。保存只产生版本，必须显式发起发布任务。
2. 发布前强制**自动快照**（先备份再动手）。
3. 每个节点独立的 `-t` 校验。校验失败 → 该节点跳过，记录原因，不 reload。
4. reload 后 3s 内做 HTTP 探活（可配置探测 URL），失败视为节点失败。
5. 批次失败率 > 阈值（默认 0%）→ 立即中止后续批次 + 自动回滚本批次及之前所有批次。

### 5.2 流程 B：证书续期（自动化）

```
[扫描到期时间] → <30天？ → [自动触发 ACME 续期] → [DNS-01 挑战] → [签发成功]
      │                                                              │
      └─ 否：等待下次扫描（每日 03:00）                     [加密入库 + 生成新版本]
                                                                     │
                                        [对绑定集群发起"仅证书"发布任务] → [reload]
                                                                     │
                                                        [结果通知 + 审计记录]
```

**关键规则**：

- 续期失败重试 3 次（间隔 1h），仍失败 → 红色告警推送负责人。
- 证书私钥在库中以 AES-256-GCM 加密存储，主密钥由平台 KMS/环境变量注入。
- 证书发布是"轻量发布"：不重启进程，仅 `reload`。

### 5.3 流程 C：LVS+DR 摘除故障节点

```
[监控发现 RS 异常] → [运维在 LVS 页选 RS] → [权重置 0 / 摘除]
        │                                            │
        └─ 自动生成两笔变更：                    [下发 ipvsadm + 更新 keepalived.conf]
           1) LVS 层：RS 摘除
           2) Nginx 层：从对应 upstream 移除该节点（可选，勾选联动）
                                                     │
                                        [走统一发布流水线：校验→灰度→观测→完成]
                                                     │
                                              [拓扑图更新 + 通知]
```

**关键规则**：

- LVS 变更与 Nginx 变更**共享同一条流水线**，但目标类型不同（ipvsadm 命令 vs 配置文件）。
- DR 模式下 RS 必须配置 `lo:0` 绑定 VIP + `arp_ignore/arp_announce`，平台在节点纳管时自检这两项，不合规直接告警。
- 任何 `ipvsadm` 变更前先 `ipvsadm-save` 快照，回滚时直接 `ipvsadm-restore`。

### 5.4 流程 D：应急回滚

```
[发现故障] → [发布任务页 / 备份页] → [选择目标版本或快照] → [选择回滚范围]
                                                                │
                                              [标准流水线执行（跳过编辑）]
                                                                │
                                                  [完成 + 强制刷新漂移检测]
```

**SLA 目标**：从决定回滚到全部节点生效 ≤ 60 秒（100 节点规模内）。

---


## 6. 数据模型（核心实体）

| 实体                           | 关键字段                                                                                                                                                      | 关系                      |
| ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------- |
| **Cluster（集群/分组）**           | id, name, env(prod/stg/dev), idc, tags[], vars(JSON), lvs_enabled                                                                                         | 1:N → Node              |
| **Node（节点）**                 | id, hostname, ip, ssh_port, cluster_id, agent_version, nginx_version, status(online/offline/unknown), last_heartbeat, config_hash, vars(JSON), dr_checked | N:1 → Cluster           |
| **ConfigFile（配置文件）**         | id, path, cluster_id(可空=全局), content, is_template, current_version                                                                                        | 1:N → ConfigVersion     |
| **ConfigVersion（配置版本）**      | id, config_file_id, version_no, content, git_sha, author, change_note, created_at                                                                         | N:1 → ConfigFile        |
| **TemplateVar（变量）**          | id, scope(global/cluster/node), scope_id, key, value, is_secret                                                                                           | —                       |
| **DeployTask（发布任务）**         | id, title, type(config/cert/lvs/rollback), cluster_id, strategy(JSON), status, operator, approver, created_at, finished_at                                | 1:N → DeployBatch       |
| **DeployBatch（批次）**          | id, task_id, seq, node_ids[], status, started_at, finished_at                                                                                             | 1:N → DeployNodeResult  |
| **DeployNodeResult（节点结果）**   | id, batch_id, node_id, steps, status, error, duration_ms                                                                                                  | —                       |
| **Snapshot（快照）**             | id, node_id or cluster_id, type(pre_deploy/manual/scheduled), archive_path, size, created_at                                                              | —                       |
| **Certificate（证书）**          | id, common_name, san[], issuer, not_before, not_after, encrypted_key, chain, acme_order(JSON), auto_renew, bound_clusters[]                               | N:M → Cluster           |
| **LvsDirector**              | id, node_id, vrrp_role(MASTER/BACKUP), priority, virtual_router_id, status                                                                                | 1:N → LvsVirtualService |
| **LvsVirtualService**        | id, director_id, vip, port, protocol, scheduler, persistence_timeout, fwmark                                                                              | 1:N → LvsRealServer     |
| **LvsRealServer**            | id, vs_id, node_id(可选), ip, port, weight, enabled, health_status                                                                                          | N:1 → VS / N:1 → Node   |
| **AuditLog**                 | id, actor, action, target_type, target_id, before, after, result, ip, ua, created_at                                                                      | —                       |
| **User / Role / Permission** | 标准 RBAC 三表                                                                                                                                                | —                       |

---

## 7. 非功能需求

| 类别 | 指标                     | 目标值                                                  |
| -- | ---------------------- | ---------------------------------------------------- |
| 规模 | 单平台纳管节点数               | ≥ 500                                                |
| 规模 | 单集群节点数                 | ≥ 100                                                |
| 性能 | 100 节点全量发布耗时（3 批次）     | ≤ 3 分钟                                               |
| 性能 | 单节点变更（备份→下发→校验→reload） | ≤ 8 秒                                                |
| 性能 | 回滚全量生效                 | ≤ 60 秒                                               |
| 可用 | 控制面可用性                 | ≥ 99.9%（控制面挂了不影响数据面，Nginx 照常服务）                      |
| 可靠 | 发布成功率（排除配置本身错误）        | ≥ 99.99%                                             |
| 安全 | 传输                     | 全链路 TLS 1.2+；Agent↔Server mTLS                       |
| 安全 | 存储                     | 私钥/令牌 AES-256-GCM 加密；审计日志只追加不可改                      |
| 安全 | 权限                     | 最小权限；生产集群变更强制审批 + 二次确认                               |
| 兼容 | Nginx                  | 1.18+ / OpenResty 1.19+ / Tengine 2.3+               |
| 兼容 | 系统                     | CentOS 7/8、Rocky 8/9、Ubuntu 20.04/22.04、Debian 11/12 |
| 兼容 | LVS                    | ipvsadm 1.27+ / keepalived 2.0+（DR / NAT / TUN）      |

---

## 8. 里程碑排期（建议）

| 阶段 | 周期 | 交付物 | 验收标准 |
| --- | --- | --- | --- |
| **M0 项目地基** | W1 前 2 天 | 骨架/配置/DB schema/错误处理 | `make test` 全绿，PG + SQLite 双跑通 |
| **M1 骨架与接入** | W1–W2 | gRPC/mTLS/Agent 注册·心跳·能力发现 | 4 节点在线，掉线 30s 内感知 |
| **M2 配置中心** | W3–W4 | 配置树/版本链/Diff/校验/漂移 | 改 conf 看 diff 与校验；写错被拦 |
| **M3 发布引擎** ★ | W5–W6 | 变更单/快照/原子落盘/探活/回滚/LVS灰度 | 错配→零污染→自动回滚 |
| **M4 证书管理** | W7 | ACME/CF/上传校验/分发/续期 | 测试证书自动续期分发 |
| **M5 LVS 管理** | W8 | Keepalived渲染/DR巡检/拓扑/权重 | 改坏 `arp_ignore` 被阻断 |
| **M6 日志与安全** | W9–W10 | 采集/ClickHouse/检索/TraceID/规则/封禁 | request_id 查全链路；注入封禁可回滚 |
| **M7 监控** | W11 | Agent metrics/VM/Grafana/告警汇聚 | Grafana 有数；告警汇总中心 |
| **M8 构建与升级** | W12 | 版本矩阵/构建/热升级/LVS联动 | nginx 补丁升级 zero 5xx |
| **M9 备份与运维** | W13 | 备份/恢复演练/设置/审计 | 恢复演练通过（PITR） |

> 关键路径：M0→M1→M2→M3→（M4/M5/M6/M7/M8 可并行）→M9。
> **最小可用闭环 = M0+M1+M2+M3**，做完即可安全管配置，其余为增值模块，应先用起来再迭代。

> 排期按 **1 名全栈（Go + Vue）全职**估算，约 13 周。规模按「2 Director + 2 RS」校准；数据库主选 **PostgreSQL**（开发态可用 SQLite 同构），仍不引入 Redis / K8s。
> 完整选型理由见 `docs/DECISIONS.md`（§8 容量 / §9 数据库 / §10 时序 / §11 监控 / §12 构建升级）。

---

## 8.5 容量评估与资源预算

**结论先行**：百万级日访问（约 10–20 RPS 均值、峰值数百 RPS）下，**2 LVS Director + 2 Nginx RS 足够**，瓶颈不在算力而在单条 TCP 流的万兆上限与后端应用。后期扩展优先"加 RS / 升规格"，而非加 LVS 层。

| 维度 | 评估 | 扩容路径 |
| --- | --- | --- |
| 连接/吞吐 | 2×Nginx 1.30（编译含 threads+aio+http_v3）轻松扛百万级 | 先加 RS 到 4 台；仍不够再升 vCPU |
| LVS 层 | 2 Director 主备冗余充足，DR 模式接近线速 | 极少需要加 Director |
| 单流带宽 | 单条 TCP 流上限 = 单万兆链路 10G（LAG 不叠加单流） | 大文件场景注意；多流自然分散 |
| 后端瓶颈 | 真实瓶颈通常是 upstream 应用，非 Nginx | 平台提供 upstream 耗时透视（TraceID）定位 |

**本地资源（2× TH-D2110，ESXi 虚拟化，全万兆 VDS LACP）**：

| 角色 | vCPU | 内存 | 备注 |
| --- | --- | --- | --- |
| 控制面（NGX-CP） | 2 | 4G | 绝对资源预留（成本低，稳定性优先） |
| PostgreSQL | 2 | 4G | WAL 归档 + 每日 dump |
| ClickHouse | 4 | 6G（限 `max_memory_usage`） | TTL 7 天，本地 25T 充裕 |
| Prometheus + Grafana | 2 | 4G | 单实例足够 |
| Director ×2 | 1+1 | 1G+1G | **全部预留**（vMotion/DRS 不挤压） |
| RS（Nginx）×2 | 4+4 | 4G+4G | 预留 50%，业务弹性 |
| 合计 | ≤ 24 vCPU / ≤ 28G | 112 核 / 128G 资源极宽裕 | Director 必须设反亲和性规则 |

> 详见 `docs/DECISIONS.md` §8（容量）、§14（资源预算）、§16（vSphere/万兆）。

## 9. 风险与对策

| 风险                                | 影响 | 对策                                                                                          |
| --------------------------------- | -- | ------------------------------------------------------------------------------------------- |
| 控制面成为单点，挂了就改不了配置                  | 高  | 控制面只做编排，不参与流量转发；Nginx 数据面完全自治，控制面宕机业务无感。控制面自身做主备或容器化编排                                      |
| Agent 被控后可任意写节点文件                 | 高  | mTLS + 节点令牌绑定 IP/指纹；Agent 只接受控制面下发的**签名指令**；指令白名单（只能写 `/etc/nginx/**`、`/etc/keepalived/**`） |
| 批量变更放大故障                          | 高  | 强制灰度 + 批次失败率熔断 + 并发变更锁（同一集群同时只允许 1 个发布任务）                                                   |
| `nginx -t` 通过但语义错误（如 upstream 全挂） | 中  | 语义规则引擎 + 发布期 HTTP 探活 + 发布后 5xx 率突增自动告警                                                      |
| 模板渲染导致节点配置意外变化                    | 中  | 发布前展示**每个节点的渲染结果 Diff**，逐节点确认                                                               |
| LVS DR 模式 ARP 配置遗漏导致不通            | 中  | 节点纳管时自动检查 `arp_ignore=1` / `arp_announce=2` / `lo:0` 绑定 VIP，不合规标红并阻断 LVS 发布                 |
| 证书私钥泄露                            | 高  | 加密存储 + 私钥不进日志/不进前端响应 + 下载需审批留痕                                                              |
| 配置仓库无限膨胀                          | 低  | Git 仓库定期 gc + 快照归档按保留策略（默认保留 90 天 / 最近 200 份）                                               |

---

## 10. 自研 vs 现成方案对比

| 方案                  | 优势                                  | 劣势                              | 结论            |
| ------------------- | ----------------------------------- | ------------------------------- | ------------- |
| **自研 NGX-CP（本方案）**  | 完全贴合 LVS+DR 场景；Nginx/LVS 统一编排；可控可演进 | 需投入开发                           | ✅ 推荐          |
| Nginx Proxy Manager | 开箱即用、证书自动                           | 面向单/少量节点，无集群灰度、无 LVS            | ❌ 不满足         |
| Ansible / SaltStack | 成熟、agentless                        | 无可视化、无版本 diff UI、无证书生命周期、回滚需自己写 | 🟡 可作为底层执行器复用 |
| 宝蓝德 Nginx 管理 / 商业产品 | 功能全                                 | 闭源收费、定制难、LVS 场景不一定有             | ❌ 成本高         |
| 直接上 K8s Ingress     | 生态好                                 | 与现有裸机集群架构不兼容，迁移成本巨大             | ❌ 场景不符        |

> **折中建议**：控制面自研，执行层复用 Ansible 的 `copy/systemd/nginx` 模块思路，但不引入 Ansible 依赖（Agent 直接执行，链路更短、排障更简单）。

---

## 11. 成功指标（上线 3 个月后衡量）

| 指标            | 现状                 | 目标             |
| ------------- | ------------------ | -------------- |
| 单次配置变更平均耗时    | ~20 min（含登录、同步、验证） | ≤ 3 min        |
| 因配置错误导致的线上事故数 | N 次/季              | 0 次（校验 + 灰度拦截） |
| 证书到期未续导致的中断   | N 次/年              | 0 次            |
| 故障回滚耗时 MTTR   | ~15 min            | ≤ 1 min        |
| 配置漂移节点占比      | 未知（不可观测）           | 可见且趋近 0        |
| 变更审计覆盖率       | 0%                 | 100%           |

---

## 12. 关键决策结论（已确认）

环境基线：**2 台 Keepalived（主备）+ 2 台 Nginx 1.30.0（DR 模式 RS）、CentOS 系、编译安装、Cloudflare DNS、自用规模。**

| # | 决策点 | 结论 | 详见 |
| - | ----- | ---- | ---- |
| 1 | 节点接入 | **Agent 常驻**（mTLS 主动外连），跨机房/NAT 免开入站端口 | DECISIONS §1.5 |
| 2 | 配置同步传输 | **Agent 内建传输**为默认；rsync 仅用于大体积/目录树；**弃用 scp**（协议已 deprecated） | DECISIONS §1 |
| 3 | 配置存储 | **PostgreSQL `config_blob` 内容寻址 + `config_revision` 血缘链**（开发态可用 SQLite 同构）；可选导出 Git 裸仓。**节点上从不需要 Git** | DECISIONS §9 |
| 4 | 数据库 | **PostgreSQL 16（主库）+ SQLite（开发态同构 fallback）**，不引入 Redis；备份 `pg_dump` + WAL 归档 + 异地 | DECISIONS §9 |
| 5 | 灰度策略 | **1+1 串行**，且走 **LVS 权重摘除**（先摘 RS2 → 排空 → 变更 → 探活 → 加回 → 再动 RS1） | DECISIONS §4.3 |
| 6 | 日志统一 | TraceID 透传 + 标准 JSON 格式 + **Agent 内置采集** + ClickHouse（TTL 7 天） | DECISIONS §3 |
| 7 | 攻击预警 | 10 条滑动窗口规则；**封禁走发布流水线**（可回滚/可灰度/有审批），不新开写线上的通道 | DECISIONS §3.5 |
| 8 | 证书来源 | **ACME（Cloudflare DNS-01）+ 手动上传**，二者都要；上传做 6 项校验 | DECISIONS §5 |
| 9 | DNS Provider | 接口抽象，v1 实现 Cloudflare，预留 Aliyun（曾用） | DECISIONS §5.3 |
| 10 | 权限对接 | **本地账号 + TOTP**，3 角色；用户表预留 `external_id` | DECISIONS §7 |

### 12.1 前期待确认项（已全部闭环）

1. ~~拓扑~~ → **Keepalived 独立 2 台 Director**（已确认，非与 Nginx 同机）。
2. ~~控制面位置~~ → **本地 2 台 TH-D2110 虚拟化环境内部署**（vSphere，全万兆，Agent 内网稳定连回）。
3. ~~日志量级~~ → 本地资源极宽裕（25T / 128G），**直接上 ClickHouse**（单实例 + TTL 7 天），不走低配方案。

> 唯一仍建议持续观察的是**实际日 PV**：若未来超千万级，再评估 ClickHouse 分片或 VictoriaLogs；当前架构不变。
