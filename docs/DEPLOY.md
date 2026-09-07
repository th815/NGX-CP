# 部署指南（控制面 + 节点 Agent）

> 本文档为**环境无关**的通用部署指南，不绑定任何具体主机 / IP / 内网信息。
> 主机相关的具体部署记录（目标地址、token 保管、回滚时间点等）请写在本地任务日志（`.workbuddy/memory/`，已被 gitignore，**不入库**），不要提交到仓库或写入根 README。

## 1. 形态

- 单 Go 二进制 + 内嵌前端 SPA（`web/dist` 经 `//go:build webui` 打进二进制），无需独立静态服务。
- SQLite 单实例（开发/小规模）；生产可切换 PostgreSQL（见 `internal/repo`）。
- 以 systemd 托管（`scripts/ngxcp-server.service`，`User=root`），`db_auto_migrate=true` 首启建表，PKI 自动创建。

## 2. 部署步骤（可重复，`scripts/deploy.sh`）

1. 本地构建前端：`cd web && npm run build`（含 `vue-tsc` 类型检查）产出 `web/dist`。
2. 交叉编译：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags webui -o bin/ngxcp-server ./cmd/ngxcp-server`。
3. 传输：经 SSH 把二进制、`configs/rules.yaml`、`scripts/ngxcp-server.service` 送到目标主机（目标由环境变量 `NGXCP_DEPLOY_HOST` 指定，例如 `NGXCP_DEPLOY_HOST=root@your-host bash scripts/deploy.sh`）。
4. 安装：备份旧二进制到 `<部署目录>/backups/ngxcp-server.<时间戳>` → 落位 → **首启在目标主机本地生成 `config.yaml`**（随机 `auth_admin_token` / `security_session_secret`，**绝不入库、绝不覆盖**）→ 配置自检 → `daemon-reload && enable && restart`。
5. 冒烟：`/health`、`/api/v1/version`、`/`（SPA 首页 200）三连。

## 3. 机密管理（重要）

- `config.yaml` 仅在**首次部署**由目标主机侧 `openssl rand -hex 32` 生成，不进版本库。
- 重装/回滚只动二进制，不动 `config.yaml`，线上 token 稳定。
- 前端调用 `/api/v1` 使用的 Bearer token 由用户在界面录入，不经仓库。

## 4. 回滚

- 每次部署前自动备份旧二进制：`<部署目录>/backups/ngxcp-server.<时间戳>`。
- 回滚：`systemctl stop ngxcp-server` → 换回备份二进制 → `systemctl start ngxcp-server`（`config.yaml` 与数据库不动）。

## 5. 已知注意

- `enable --now` 不会重启**已在运行**的服务；脚本已改用显式 `restart`。
- 发布引擎 `Runner`（`internal/server/agent_runner.go`）**已接线**：控制面经 Agent 心跳命令通道下发
  部署 / 回滚 / 快照 / 调权指令，变更单会收敛到真实 `success` / `failed`。
  前提是**目标节点已安装并注册 Agent** —— 节点未接入时下单会明确失败（Agent 未在线），
  而不是静默停留在 running。节点部署见下一节。

## 6. 部署节点 Agent（ngxcp-agent）

Agent 常驻在每台 Nginx / Keepalived 节点上，**主动外连**控制面（gRPC + mTLS），
节点无需开放任何入站端口。

### 6.1 前置

1. 控制面已部署，且 `agent_grpc` 端口（默认 `:9443`）可从节点访问。
2. 两种接入路径：**Web 一键自注册（6.6，推荐）** 无需预建节点；**推送部署（6.2）** 需先为每节点生成一次性接入令牌并取 CA。
3. 控制面 CA 证书（`ca.crt`）由 `/agent/ca.crt` 公开提供，引导期信任用，无需手工分发。

### 6.2 部署（`scripts/deploy-agent.sh`）

先准备令牌文件（**含机密，已列入 `.gitignore`，切勿入库**），每行一条 `<ssh目标>=<一次性令牌>`：

```bash
# .agent-tokens
root@rs1=<token-1>
root@rs2=<token-2>
```

再执行 —— 主机、控制面地址、CA 路径全部由环境变量提供，脚本不含任何内置环境信息：

```bash
NGXCP_AGENT_HOSTS="root@rs1 root@rs2" \
NGXCP_AGENT_CONTROL_PLANE="cp.internal:9443" \
NGXCP_AGENT_CA_CERT="./pki/ca.crt" \
NGXCP_AGENT_TOKENS_FILE="./.agent-tokens" \
bash scripts/deploy-agent.sh
```

流程：交叉编译 → 传二进制 / unit / CA → 备份旧二进制 → `stop` → 落位 → `start` → 健康校验。
**任一节点校验失败即自动回滚该节点并中止**，不会把坏版本继续推向后续节点（灰度场景请一台一台推）。

### 6.3 机密管理

- 配置文件 `/etc/ngxcp/agent.conf`（`chmod 600`，systemd EnvironmentFile 格式）承载控制面地址与 enroll token，
  不落进 systemd unit，也不会出现在 `ps` 输出里。
- **enroll token 是一次性的**：首次注册后 Agent 会把客户端证书持久化到 `data-dir`（默认 `/var/lib/ngxcp`）。
  因此重复部署**只创建、绝不覆盖**已有 `agent.conf` —— 否则会用已失效的令牌覆盖掉有效凭据。
- 令牌文件与 CA 私钥均不入库；`.gitignore` 已增列 `.agent-tokens` / `*.tokens`。

### 6.4 回滚

- 每次部署前自动备份旧二进制到 `/opt/ngxcp/backups/ngxcp-agent.<时间戳>`。
- 单独回滚：`NGXCP_AGENT_HOSTS="root@rs1" NGXCP_AGENT_ROLLBACK=1 bash scripts/deploy-agent.sh`。

### 6.5 systemd 加固（`scripts/ngxcp-agent.service`）

- `ProtectSystem=full` + `ReadWritePaths=/etc/nginx /etc/keepalived /var/lib/ngxcp /var/log/nginx`：
  Agent 必须能改 nginx / keepalived 配置、抓快照、跑 `nginx -t` 与 `reload`，其余路径只读。
- `AmbientCapabilities=CAP_NET_ADMIN`：LVS Director 上 `ipvsadm` 调整 RS 权重所需；纯 RS 节点保留亦无副作用。
- `MemoryMax=256M`：避免 Agent 失控影响同机 nginx。
- 虚拟化环境前置（vCenter，Agent 运行时不感知，属部署清单强制项）：
  Director 端口组须开「混杂模式 + MAC 地址更改 + 伪传输」；Keepalived VRRP 必须 unicast；
  必须关闭 VMware Tools 时间同步并启用 chrony —— 详见 `docs/DECISIONS.md`。

### 6.6 控制台一键接入（推荐：走 `/nodes` 页面）

生产环境首选：**在「节点」页面直接添加节点并生成安装命令**，与其它页面风格一致（Naive UI）。

1. 浏览器打开控制面 `http://<控制面>:8080/nodes`，录入管理员 Bearer 令牌后点「添加节点」。
2. 填节点名称（唯一标识，如 `nginx-rs-01`）、选节点角色（`real_server` / `director`）、选令牌有效期
   →「登记并生成安装命令」。
   页面直接给出可复制的一行命令，控制面地址与 gRPC 端口由 `GET /api/v1/agent/bootstrap-info`
   自动填充，无需手填。
   > 备用入口：`http://<控制面>:8080/agent/` 提供同样能力（独立页面，供未加载 SPA / 离线场景）。
   控制面**先建节点（enrolling）**，再为该节点签发**节点绑定 Join Token**（内嵌 nodeID + 角色，
   默认 24h 有效），原文仅返回一次，库内只存 SHA-256 哈希 + 绑定节点（服务端 `join_tokens` 表）。
3. 复制给出的单行命令，到目标节点以 root 执行：

   ```bash
   curl -fsSL http://<控制面>:8080/agent/install.sh | sudo bash -s -- \
     --cp http://<控制面>:8080 --grpc <控制面:9443> --token <JOIN_TOKEN>
   ```

   > **注意**：控制面 HTTP 默认**明文监听 `:8080`**（仅 Agent gRPC `:9443` 走 mTLS，已加密）。
   > 生产若需 HTTPS 控制台，在前面挂你们自己管的 Nginx / Cloudflare 反代即可（产品本身就是管 Nginx 的，可自反代自身），无需改控制面代码。

   脚本会：拉取引导 CA → 按架构（amd64 / arm64）下载 Agent 二进制 → 写 systemd 单元
   （机密走 `/etc/ngxcp/agent.conf`，`chmod 600`，不进 unit、不出现在 `ps`）→ `enable --now`。
4. Agent 启动后用 Join Token + 本地生成的 CSR 自注册，**控制面复用该节点**（名称/角色取自令牌绑定节点、
   不新建节点）并签发客户端证书，节点随即上线（无审批），出现在「节点」列表。

**前置：控制面须启用二进制分发** —— `config.yaml` 的 `agent_dist_dir` 指向含
`ngxcp-agent-linux-{amd64,arm64}` 的目录（控制面在 `/agent/bin/<file>` 提供下载）。
`deploy.sh` 会自动构建上传该目录并幂等补齐配置，新部署无需手工干预。

> **[2/5] 下载 Agent 二进制 404 排查**：说明分发未启用（早期版本部署未带此能力）。
> 在能 SSH 到控制面的机器上执行一次补救即可，无需全量重部署：
>
> ```bash
> NGXCP_DEPLOY_HOST=root@<控制面> bash scripts/enable-agent-dist.sh
> ```
>
> 脚本会交叉编译并上传二进制、幂等补齐 `agent_dist_dir`、重启控制面，并验证
> `/agent/bin/ngxcp-agent-linux-amd64` 返回 200。控制台「添加节点」在检测到分发未就绪时也会给出同样提示。

**令牌模型（仿妙妙屋X）**：一个 Agent 一个 Token，令牌原文持久化于 Agent 侧 `/etc/ngxcp/agent.conf`
（systemd EnvironmentFile），控制面服务端**两张表**按哈希反查节点，均支持**单独吊销**（即时生效、无需等过期）：

- **Join Token（`join_tokens` 表）**：节点绑定、可复用。`POST /api/v1/nodes/:id/join-token` 轮换即
  吊销旧令牌；亦可经 `POST /api/v1/nodes/:id/join-token/revoke` **独立吊销**（只吊销不签发，
  用于令牌泄漏 / 节点下线安全响应）；已纳管节点可凭同一令牌**重建客户端证书**（证书丢失场景）。
- **Enroll Token（`enroll_tokens` 表）**：预建节点场景下的一次性令牌（首次注册后即作废），
  入库持久化（**重启不丢**）；经 `POST /api/v1/nodes/:id/enroll-token/revoke` 可主动吊销
  （吊销即时生效，无需等过期）。

两种令牌控制面都**只存 SHA-256 哈希 + 绑定节点 + 过期 + 吊销/已用标志**，原文仅在签发时返回一次。

## 7. 本地验证（上机前必跑）

稳定第一：推真实裸金属前，先在本地 docker 沙箱把整条链路跑通，避免「上线才发现控制面起不来」。

```bash
make build                                   # 交叉编译 linux/amd64 静态二进制到 bin/
bash scripts/verify-local.sh                # 起控制面 + Agent，验证到节点 online 自动清理
# bash scripts/verify-local.sh --keep        # 保留容器，便于 docker logs 排查
```

脚本复刻生产 Web 一键自注册流程：**控制面自举 CA → `POST /api/v1/nodes` 建节点 → `POST /:id/join-token` 签 Join Token → `GET /agent/ca.crt` 取 CA → 起 Agent → 轮询节点 `online`**。
跑通即证明部署包（镜像 / 路由 / mTLS / 自注册）端到端可用；此后才可执行 `scripts/deploy.sh` 与 `scripts/deploy-agent.sh` 推真实机。

> 注：`Dockerfile.server` / `Dockerfile.agent` 仅把 `make build` 产物 COPY 进 alpine 镜像，供验证用，
> 不承载生产编排（生产按 §2 / §6 走 `scripts/deploy*.sh`）。`configs/server.local.yaml` 为本地验证专属
> 配置（sqlite + 固定令牌），已 gitignore，不会进入仓库。

## 8. 执行清单（按此顺序推真实机）

### 8.1 前置（一次性）
- 本机 → 目标机 **root SSH 免密**（`ssh-copy-id root@<host>`，脚本用 `BatchMode=yes` 无交互）。
- 目标机为 **linux/amd64**（TH-D2110 满足）；控制面开放 **8080/9443**，节点 Agent 仅需**出方向 9443**。
- 本机装好 **go + node/npm**（`deploy.sh` 自动交叉编译 + 构建前端，目标机无需 go）。

### 8.2 推控制面（1 台）
```bash
NGXCP_DEPLOY_HOST=root@<控制面IP> bash scripts/deploy.sh
```
- 首次生成 `/opt/ngxcp/config.yaml`（**sqlite + 随机 admin token**，文件 600，未入库）。
- 脚本自带冒烟：`/health`、`/api/v1/version`、SPA `/` 全绿即成功。
- 取管理员令牌（去除 YAML 引号）：`TOKEN=$(ssh root@<控制面IP> 'grep auth_admin_token /opt/ngxcp/config.yaml' | sed -E 's/.*:[[:space:]]*"?([^"]*)"?.*/\1/')`
- 回滚：`ssh root@<控制面IP> 'systemctl stop ngxcp-server; cp -f /opt/ngxcp/backups/ngxcp-server.<时间戳> /opt/ngxcp/ngxcp-server; systemctl start ngxcp-server'`。

### 8.3 纳管节点（二选一）
**A. Web 一键（推荐，无审批，契合架构选型）**：浏览器开 `http://<控制面>:8080/agent/`，填 admin 令牌 →
填节点名/角色 →「新建节点并生成接入命令」→ 到每台目标节点以 root 执行该命令即上线。逐台操作即天然灰度。

**B. 批量（enroll token，适合一次性铺多台）**：
1) 控制面建节点 + 发 enroll token（前缀 `ngxcp_`），每条对应一台，写入 `.agent-tokens`：
```bash
TOKEN=$(ssh root@<控制面IP> 'grep auth_admin_token /opt/ngxcp/config.yaml' | sed -E 's/.*:[[:space:]]*"?([^"]*)"?.*/\1/')
for H in rs1 rs2 director1 director2; do
  NID=$(curl -fsS -X POST http://<控制面>:8080/api/v1/nodes -H "Authorization: Bearer $TOKEN" \
    -d "{\"name\":\"$H\",\"role\":\"real_server\"}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["id"])')
  curl -fsS -X POST "http://<控制面>:8080/api/v1/nodes/$NID/enroll-token?ttl=24h" -H "Authorization: Bearer $TOKEN" \
    | python3 -c 'import sys,json;print("'"$H"'="+json.load(sys.stdin)["data"]["token"])' >> .agent-tokens
done
```
2) 取 CA：`curl -fsS http://<控制面>:8080/agent/ca.crt -o pki/ca.crt`
3) 推 Agent（逐节点灰度：一台失败自动回滚该节点、不继续推后续）：
```bash
NGXCP_AGENT_HOSTS="root@rs1 root@rs2 root@director1 root@director2" \
NGXCP_AGENT_CONTROL_PLANE="<控制面>:9443" \
NGXCP_AGENT_CA_CERT="./pki/ca.crt" \
NGXCP_AGENT_TOKENS_FILE="./.agent-tokens" \
bash scripts/deploy-agent.sh
```
模板见 `.agent-tokens.example`；`NGXCP_AGENT_ROLLBACK=1` 可整体回滚到上次备份。

### 8.4 校验
```bash
curl -s http://<控制面>:8080/api/v1/nodes -H "Authorization: Bearer $TOKEN"
# 所有节点 status=online 即完成。Agent 重启复用 /var/lib/ngxcp 持久化证书，免重注册。
```

> **数据库**：脚本默认 **sqlite**（单控制面实例、2+2 规模足够，自动建表）。若按架构决策上
> **PostgreSQL 16**，先手动在控制面落一份含 `db_driver: postgres` + `db_dsn` 的 `config.yaml`
> （脚本「仅首次生成」，已有则保留），其余不变；备份改 `pg_dump` + WAL 归档。
