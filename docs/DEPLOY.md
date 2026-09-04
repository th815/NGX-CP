# 部署控制面（ngxcp-server）

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
- 真实执行（Agent 经 gRPC 落盘配置/证书）需在生产节点安装 Agent 后，由发布引擎 `Runner` 接线；在此之前变更单停留在「等待执行器接入」。
