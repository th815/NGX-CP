# 部署记录：NGX-CP 控制面 @ 192.168.5.50

> 本文档记录控制面 `ngxcp-server` 在测试/预发主机 `192.168.5.50` 的部署事实，作为 T039「发布页面 + 集成验收」完成后的**正式部署**（此前 T038 期间的安装仅作为健康检查预览）。

## 1. 主机信息

| 项 | 值 |
| --- | --- |
| 主机名 | `tianhao-test`（TH-D2110 虚拟化宿主，非生产只读区 5.6–5.9） |
| IP | `192.168.5.50` |
| OS | Rocky Linux release 9.7 (Blue Onyx) |
| 磁盘 | `/` 499G 总量 / 264G 可用（48% 已用） |
| 内存 | 15G 总量 / 12G 可用 |
| 运行时 | systemd（unit `ngxcp-server.service`，`User=root`），SQLite 单实例 |
| 访问 | `http://192.168.5.50:8080`（防火墙未启用 firewalld，端口直通） |
| 真实仓库 | `https://github.com/th815/NGX-CP`（单元 `Documentation=` 已对齐，非臆造） |

## 2. 部署形态（T039 落地后）

- **单二进制 + 内嵌前端**：`ngxcp-server` 以 `go build -tags webui` 编译，前端 SPA（`web/dist`）经 `//go:embed` 打进二进制，无需独立静态服务。
- **SPA 路由**：服务端 `web.RegisterWebUI` 提供 `NoRoute` 回退，`/`、`/deploy` 等深链返回 `index.html`；`/api`、`/health` 不参与回退。
- **状态机闭环**：服务启动时拉起 `deploy.Worker`（goroutine + `signal.NotifyContext` 同生命周期），轮询 `pending` 单 → `Service.Start` →（无 Agent 时仅置 `running` 并发「等待执行器接入」事件）→ 释放节点锁。
- **数据**：SQLite 落 `/var/lib/ngxcp/ngxcp.db`（`db_auto_migrate=true` 首启建表）；PKI 自动创建于 `/opt/ngxcp/pki`。

## 3. 部署步骤（可重复，见 `scripts/deploy-5.50.sh`）

1. 本地 `cd web && npm run build` 产出 `web/dist`（含类型检查 `vue-tsc`）。
2. 交叉编译 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags webui -ldflags ... -o bin/ngxcp-server ./cmd/ngxcp-server`。
3. `ssh root@192.168.5.50` 建目录（含 `backups/`、`snapshots/`、`artifacts/`），firewalld 未运行则跳过放通。
4. `scp` 二进制 / `configs/rules.yaml` / `scripts/ngxcp-server.service`。
5. 安装：备份旧二进制到 `/opt/ngxcp/backups/ngxcp-server.<时间戳>` → 落位 → **首启生成 `config.yaml`（机密不入库）** → `--check-config` 自检 → `daemon-reload && enable && restart`（已修正：`enable --now` 不会重启已运行服务，必须显式 `restart`）。
6. 冒烟：`/health`、`/api/v1/version`、`/`(SPA 200) 三连。

## 4. 机密管理（重要）

- `config.yaml` 仅在**首次部署**由主机侧 `openssl rand -hex 32` 生成 `auth_admin_token` 与 `security_session_secret`，**绝不入库、绝不覆盖**。
- 重装/回滚只动二进制，不动 `config.yaml`，因此线上 token 稳定。查看：`ssh root@192.168.5.50 'grep auth_admin_token /opt/ngxcp/config.yaml'`。
- 前端调用 `/api/v1` 使用的 Bearer token 由用户在界面顶栏录入，不经仓库。

## 5. 集成验收结果（T039，端到端实跑）

在 `192.168.5.50` 用真实 HTTP 走完最小可用闭环：

| 步骤 | 操作 | 结果 |
| --- | --- | --- |
| 鉴权 | 无 token 调 `POST /change-orders` | **401**（写操作鉴权生效） |
| 建单 | `POST /change-orders` | 200，状态 `draft`（id=4） |
| 提交 | `POST /:id/submit` | 200，状态 `pending_approval` |
| 自审批拦截 | 同人建单+审批 `approve` | **400**「不允许审批人审批自己提交的变更单」（T036 线上生效） |
| 审批 | 换 `approver-z` `approve` | 200，状态 `pending` |
| 执行取单 | Worker 轮询（≤2s） | 状态 `running`（发「等待执行器接入」事件，因 Agent 未接入） |
| 回滚 | `POST /:id/rollback`（running 允许） | 200，状态 `rolling_back` |

> 说明：生产态 `Runner` 为 `nil`（Agent 尚未下发到 Nginx 节点 5.8/5.9，属生产只读环境待授权）。闭环在「审批通过 → 等待执行器」处按设计暂停，待 M4+ 部署 Agent 后订单将自动收敛 `success`/`failed`。

## 6. 回滚

- 每次部署前自动备份旧二进制：`/opt/ngxcp/backups/ngxcp-server.<时间戳>`。
- 回滚：`systemctl stop ngxcp-server` → `cp /opt/ngxcp/backups/ngxcp-server.<时间戳> /opt/ngxcp/ngxcp-server` → `systemctl start ngxcp-server`。（`config.yaml` 与 SQLite 不动。）
- 历次备份：`ngxcp-server.20260904-113049`（T038 预览，无 webui）、`ngxcp-server.20260904-114248`（T039 初版，无 Worker 启动）。

## 7. 已知问题

- `internal/agent` 包 `TestHeartbeater_WatchConfigChanges` 为**既有偶发失败**（文件监听时序，zsh/macOS 下约 2/3 概率失败），与 T039 改动无关，未触碰该包。T039 相关的 `deploy`/`server`/`handler` 包测试全部通过。
```
