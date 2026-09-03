# M9 · 备份与运维（W13）

> **目标**：可靠备份（PG + 快照 + 证书）+ 恢复演练 + 系统设置 + 审计 + 通知，收尾发布。
> **完成标志**：恢复演练通过（PITR）；任意操作可在审计日志追溯到人/时间/前后值。
>
> 决策依据：`docs/DECISIONS.md` §9（存储：PG 主 + SQLite 开发态）、§11（通知）。
> 注：第二轮的"单文件 cp 走天下"已被修正——**主库用 PG，备份走 `pg_dump` + WAL 归档 + 对象/SMB 异地**，SQLite 仅开发态。

---

## T090 · 备份脚本

**目标**：PG 逻辑备份 + WAL 归档 + 配置快照 tar + 证书加密备份，落本地 + 异地。

**依赖**：T006, M3(T034 快照)

**涉及文件**：
```
scripts/backup.sh
internal/backup/scheduler.go
deploy/backup/cron
```

**契约**：
```bash
#!/usr/bin/env bash
set -euo pipefail
# 1. pg_dump → /var/backups/ngxcp/pg-$(date +%F).sql.gz
# 2. WAL 归档（continuous archiving，支持 PITR）
# 3. 配置快照 tar（来自 M3 的 /var/lib/ngxcp/snapshots）
# 4. 证书加密备份（用 T040 的 KMS 信封）
# 5. 异地：rsync 到备份机 / 对象存储（校验 sha256）
```

**验收命令**：
```bash
./scripts/backup.sh && ls -lh /var/backups/ngxcp/
# 期望：pg dump + wal + 快照 + 证书 四类产物齐全，sha 清单存在
```

**AI 陷阱**：
- PG 备份**必须配合 WAL 归档**才能 PITR，光 `pg_dump` 只能恢复到 dump 时刻
- 证书备份要加密，明文备份等于泄露私钥
- 异地备份要校验 sha，否则恢复时发现损坏

---

## T091 · 恢复演练（PITR）

**目标**：提供恢复脚本 + dry-run，定期演练，验证可恢复到任意时间点。

**依赖**：T090

**涉及文件**：
```
scripts/restore.sh
docs/ops/recovery.md
```

**契约**：
```bash
./scripts/restore.sh --dry-run --target-time "2026-09-03 10:00:00"
# dry-run 只打印将要执行的步骤，不落盘
./scripts/restore.sh --target-time "..."   # 真实恢复
# 恢复后：控制面启动 + 4 节点重新握手 + 配置与证书一致
```

**验收命令**：
```bash
./scripts/restore.sh --dry-run   # 期望：打印步骤无报错
# 在隔离环境跑真实恢复，验证节点重新纳管
```

**AI 陷阱**：
- 恢复后**必须**重新跑能力发现（T013）与 DR 合规（T052），配置可能已漂移
- PITR 的 target-time 要在 WAL 覆盖范围内，超范围报错而非静默
- 演练环境要隔离，别把生产库冲了

---

## T092 · 系统设置页

**目标**：租户级全局配置（保留策略、通知渠道、认证、资源预留）。

**依赖**：T006, M0(config)

**涉及文件**：
```
web/src/views/settings/{General,Retention,Notification,Auth}.vue
internal/server/handler/settings.go
```

**契约**：
```go
type SystemSettings struct {
    LogRetentionDays   int      // 默认 7（ClickHouse TTL）
    SnapshotRetention  string   // "90d / 200 份"
    NotifyChannels     []string // 钉钉/飞书/企微/Webhook
    AuthTOTPRequired   bool     // 默认 true（面板能改线上，强开 2FA）
    ResourceReserve    bool     // Director 全部预留（见 §16）
}
```

**验收命令**：
```bash
cd web && npm run build && npm run typecheck
# 期望：构建通过；2FA 开关默认开启且可配置
```

**AI 陷阱**：
- 2FA 默认开启，这个面板能改线上配置，被拿下等于全站沦陷
- Director VM 资源预留是虚拟化环境稳定性前提（见 DECISIONS §16/§14）

---

## T093 · 审计日志

**目标**：全量操作留痕（谁/何时/改了什么/前后值），不可篡改。

**依赖**：T006, M2(config), M3

**涉及文件**：
```
ent/schema/audit_log.go
internal/audit/middleware.go       # HTTP + gRPC 拦截
internal/server/handler/audit.go
```

**契约**：
```go
type AuditLog struct {
    ID        int
    Actor     string   // 操作人（来自 2FA 登录态）
    Action    string   // "config.update" | "deploy" | "cert.upload" | "block"
    Target    string   // 节点/证书/变更单
    Before, After string // 关键字段前后值（JSON）
    IP        string
    CreatedAt time.Time
}
// 写即追加，不提供删除/修改接口
```

**验收命令**：
```bash
curl -s localhost:8080/api/v1/audit | jq '.data.items | length'
# 期望：>= 1，且含本次会话的配置/发布操作
```

**AI 陷阱**：
- 审计写**追加 only**，不提供 delete/update 接口（防篡改）
- 前后值要存关键字段，别只存"操作了"没存"改成啥"
- 审计本身要防注入，Before/After 当数据不是代码

---

## T094 · 通知中心

**目标**：钉钉/飞书/企微/Webhook 多渠道，事件总线驱动。

**依赖**：T092, M3, T067

**涉及文件**：
```
internal/notify/bus.go
internal/notify/channels/{dingtalk,feishu,wecom,webhook}.go
internal/notify/notify_test.go
```

**契约**：
```go
type Event struct {
    Type   string   // "deploy.done" | "cert.expire" | "node.offline" | "dr.fail" | "security.critical"
    Level  string
    Payload map[string]any
}
func (b *Bus) Publish(e Event)  // 按订阅渠道 fan-out
```

**验收命令**：
```bash
go test ./internal/notify/...
# 期望：用 mock webhook 验证事件正确 fan-out
```

**AI 陷阱**：
- Webhook 要带签名/鉴权，别裸奔接收
- 通知失败要重试但限次，别死循环刷屏
- 敏感事件（安全封禁）通知要脱敏，别把 IP/ payload 全量外发

---

## T095 · 发布 v0.1 + 文档收尾

**目标**：版本标记、部署文档、升级指南，交付首个可用版本。

**依赖**：T090–T094

**涉及文件**：
```
README.md                       # 项目入口
deploy/                         # docker-compose / systemd / vCenter 清单
docs/ops/{deploy,recovery,monitoring}.md
CHANGELOG.md
```

**契约 / 验收**：
```bash
git tag v0.1
make build && make test && make e2e   # 全绿
# 部署文档含：vCenter 端口组三项策略、时钟同步、PG 备份、2FA 开启
```

**AI 陷阱**：
- 部署文档**必须把 vCenter 端口组三策略 + 时钟同步关 VMware Tools 写进前置清单**，漏了 LVS 不通且极难排查
- CHANGELOG 写"已知限制"，别吹全功能

---

## 收尾 · 全局验收（M0–M9 全跑完）

```bash
make test && make lint && make e2e
# M1: 4 节点在线，掉线 30s 感知
# M3: 错配 → 零污染 → 自动回滚
# M4: 证书自动续期分发
# M5: arp_ignore 改坏被阻断发布
# M6: request_id 全链路 + 注入封禁可回滚
# M7: Grafana 有数 + 时钟告警
# M8: 升级 zero 5xx
# M9: 恢复演练通过
```

**交付物清单**：
- `AGENTS.md` · `docs/PRD.md` · `docs/ARCHITECTURE.md` · `docs/DECISIONS.md`
- `docs/tasks/README.md` + `M0`–`M9`
- `prototype/index.html`（12 页交互原型）
- `ent/` `internal/` `web/` `proto/` `deploy/`（按里程碑逐步实现）
