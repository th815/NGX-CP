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

- 环境文件 `/etc/ngxcp/agent.env`（`chmod 600`）承载控制面地址与 enroll token，
  不落进 systemd unit，也不会出现在 `ps` 输出里。
- **enroll token 是一次性的**：首次注册后 Agent 会把客户端证书持久化到 `data-dir`（默认 `/var/lib/ngxcp`）。
  因此重复部署**只创建、绝不覆盖**已有 `agent.env` —— 否则会用已失效的令牌覆盖掉有效凭据。
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

### 6.6 Web 一键自注册（推荐）

生产环境首选：无需预先在控制面建节点，也无需手工拉令牌 / CA。

1. 浏览器打开控制面 `https://<控制面>/agent/`，填管理员 Bearer 令牌（与 API 写接口同一令牌）。
2. 选节点角色（`real_server` / `director` / `director_and_rs`）→「生成接入令牌」。
   控制面返回一次性 **Join Token**（内嵌角色，默认 1h 有效，用后即焚）。
3. 复制给出的单行命令，到目标节点以 root 执行：

   ```bash
   curl -fsSL https://<控制面>/agent/install.sh | sudo bash -s -- \
     --cp https://<控制面> --grpc <控制面:9443> --token <JOIN_TOKEN>
   ```

   脚本会：拉取引导 CA → 按架构（amd64 / arm64）下载 Agent 二进制 → 写 systemd 单元
   （机密走 `/etc/ngxcp-agent.env`，`chmod 600`，不进 unit、不出现在 `ps`）→ `enable --now`。
4. Agent 启动后用 Join Token + 本地生成的 CSR 自注册，控制面**自动建节点**（名称 = hostname、角色取令牌）
   并签发客户端证书，节点随即上线，出现在「节点」列表。

前置：`make dist` 已把 Agent 二进制放入 `dist/agent/`（控制面在 `/agent/bin/` 提供下载）；
`agent_dist_dir` 为空则禁用二进制下载（此时改用 6.2 推送或手动分发）。
Join Token 与 enroll token 同生命周期（当前为内存态，控制面重启即失效，持久化随 T014 落地）。
