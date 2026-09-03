# M7 · 监控（W11）

> **目标**：统一可观测性——Agent 暴露指标 + VM 监控 + Grafana 看板 + 告警汇聚到平台告警中心。
> **完成标志**：Grafana 有数据；平台告警中心汇总 Prometheus 告警；节点时钟偏差 > 1s 告警。
>
> 决策依据：`docs/DECISIONS.md` §10（时序库）、§11（监控：不自研采集，自研业务视角）。
> 结论：**用 Prometheus + Grafana 做采集与可视化（成熟、省事），平台只自研"业务视角"指标与告警汇聚层，不重复造轮子。**

---

## T071 · Agent /metrics 暴露

**目标**：Agent 暴露 Prometheus 格式指标（stub_status + 系统 + 进程）。

**依赖**：M1

**涉及文件**：
```
internal/agent/metrics.go
internal/agent/metrics_test.go
```

**契约**：
```go
// /metrics 端点（仅控制面可访问）
# HELP nginx_connections_active ...
# TYPE nginx_connections_active gauge
nginx_connections_active{node="nginx-01"} 12
# HELP ngxcp_agent_uptime_seconds ...
ngxcp_agent_uptime_seconds{node="nginx-01"} 86400
// 取自 stub_status：active/reading/writing/waiting；系统：CPU/内存/磁盘；进程：nginx 存活
```

**验收命令**：
```bash
curl -s localhost:9101/metrics | grep -E "nginx_connections|ngxcp_agent" | head
# 期望：有输出且 node label 正确
```

**AI 陷阱**：
- 指标名用 `_total`/`_seconds`/`_bytes` 等 Prometheus 约定后缀
- stub_status 的路径从能力发现（T013）拿，别硬编码 `/stub_status`

---

## T072 · Prometheus 抓取 + 自研业务指标

**目标**：Prometheus 抓 Agent/metrics；平台自研业务指标（发布成功率、漂移数、证书到期、合规）。

**依赖**：T071, M3, T040, T052

**涉及文件**：
```
deploy/prometheus/prometheus.yml
internal/metrics/business.go      # 平台侧暴露 /metrics
```

**契约**：
```go
// 平台业务指标（Prometheus 抓平台自身）
ngxcp_deploy_success_total        // 发布成功次数
ngxcp_deploy_failure_total
ngxcp_config_drift_nodes          // 当前漂移节点数
ngxcp_cert_days_left{min="7"}     // 证书剩余天数
ngxcp_dr_compliance_fail          // DR 合规失败节点数
```

**验收命令**：
```bash
curl -s localhost:8428/api/v1/query?query=ngxcp_config_drift_nodes | jq '.data.result'
# 期望：返回当前漂移数（0 或 N）
```

**AI 陷阱**：
- 业务指标用 `Gauge`/`Counter`，别用 `Histogram` 存比率
- 证书剩余天数用 `min="7"` 触发告警，别用绝对值

---

## T073 · VM 监控（vSphere 环境）

**目标**：采集每台 VM 的 CPU/内存/网络（万兆/LACP 链路），融入统一看板。

**依赖**：T071

**涉及文件**：
```
deploy/prometheus/vsphere.yml      # vSphere Exporter 或 node_exporter
docs/ops/monitoring.md
```

**契约**：
```yaml
# vSphere Exporter 抓取 ESXi 上各 VM 指标
# 关键：万兆链路利用率、LACP 成员状态、VM 资源预留命中率
```

**验收命令**：
```bash
curl -s localhost:8428/api/v1/targets | jq '.data.activeTargets[].scrapePool' | sort -u
# 期望：含 vsphere / node / ngxcp
```

**AI 陷阱**：
- vSphere Exporter 需要 vCenter 只读账号，权限最小化
- 万兆 LACP 的**单流上限 = 单链路 10G**，监控要区分"聚合利用率"和"单流瓶颈"
- LAG 哈希算法若为源虚拟端口则只用到一条 10G，监控要能看出（见 DECISIONS §16）

---

## T074 · Grafana 数据源 + 看板

**目标**：接 Prometheus 数据源，建 4 张看板（总览 / Nginx / LVS / VM）。

**依赖**：T072, T073

**涉及文件**：
```
deploy/grafana/provisioning/...    # 数据源 + 看板自动配置
deploy/grafana/dashboards/*.json
```

**契约**：
```json
// 看板经 Terraform/API 导入，不手动点
// 总览：节点在线状态、QPS、5xx 率、发布成功率
// Nginx：连接数、 upstream 健康、TLS 握手耗时
// LVS：VIP 流量、RS 权重、DR 合规状态
// VM：CPU/内存/万兆链路利用率
```

**验收命令**：
```bash
curl -s localhost:3000/api/search | jq -r '.[].title'
# 期望：4 张看板存在
```

**AI 陷阱**：
- 看板用 provisioning 导入，**不要人工在 UI 点**，否则环境漂移
- 图表单位（ms / % / req/s）要正确，否则误导决策

---

## T075 · 告警规则 + 汇聚到平台

**目标**：Prometheus 告警规则 + Alertmanager → 平台告警中心（复用 M6 事件模型）。

**依赖**：T072, T067

**涉及文件**：
```
deploy/prometheus/rules/*.yml
internal/alert/bridge.go           # Alertmanager webhook → SecurityEvent/Alert
```

**契约**：
```yaml
# prometheus rules
- alert: Nginx5xxSpike
  expr: rate(nginx_http_requests_total{status=~"5.."}[5m]) / rate(nginx_http_requests_total[5m]) > 0.05
  for: 5m
  labels: {severity: critical}
```
```go
// Alertmanager webhook → 写入平台告警表，与 M6 安全事件同一中心展示
func (b *Bridge) OnAlert(a Alert) error { upsertAlert(a) }
```

**验收命令**：
```bash
# 触发一次 5xx  spike，观察平台告警中心出现条目
curl -s localhost:8080/api/v1/alerts | jq '.data.items | length'
# 期望：>= 1
```

**AI 陷阱**：
- 告警 `for` 要够长，避免抖动误报（尤其 vMotion 期间）
- 告警去重要靠 `alertname+instance` 稳定 label，别用时间戳

---

## T076 · 监控中心 UI

**目标**：Grafana 内嵌 + 自研业务指标卡片 + 告警规则表。

**依赖**：T074, T075

**涉及文件**：
```
web/src/views/monitor/{Overview,Embed}.vue
web/src/components/monitor/BizCard.vue
```

**契约**：
```vue
<!-- Grafana 用 iframe 内嵌（匿名只读账号），业务卡片用平台 /metrics 直出 -->
<iframe :src="grafanaUrl" />   <!-- 注意加 ?orgId=&kiosk -->
```

**验收命令**：
```bash
cd web && npm run build && npm run typecheck
# 期望：构建通过；监控页 iframe 能加载 Grafana
```

**AI 陷阱**：
- Grafana iframe 用**匿名只读** org，不能暴露编辑权限
- 业务卡片走平台 API，别直接查 Prometheus（前端跨域 + 暴露查询能力）

---

## T077 · 时钟同步校验（虚拟化特有）

**目标**：chrony 强制 + 关闭 VMware Tools 时间同步 + 偏差 > 1s 告警；vMotion 容忍。

**依赖**：T071

**涉及文件**：
```
deploy/scripts/timesync.sh
internal/monitor/clock.go
```

**契约**：
```go
// 每节点上报自己的 UTC 偏移；平台算两两偏差
func CheckClockSkew(reports []NodeReport) []Alert
// 偏差 > 1s → WARN；> 5s → CRITICAL
// 必须关闭 VMware Tools 时间同步（两者会互相纠正导致时钟跳变，破坏日志时序）
```

**验收命令**：
```bash
# 在一台 VM 上故意偏移时钟
timedatectl set-time "+2hour" 2>/dev/null || date -s "+2 hour"
# 期望：平台 CRITICAL 时钟告警；恢复后告警清除
```

**AI 陷阱**：
- **必须关 VMware Tools 时间同步**，否则与 chrony 互殴，日志时间乱序
- 跨节点日志检索（T064）依赖时钟同步，偏差 > 1s 这功能就废了
- vMotion 期间时钟可能抖动，告警去抖窗口 > 3s

---

## T078 · 监控集成验收

**目标**：端到端验证监控闭环。

**依赖**：T071–T077

**涉及文件**：
```
scripts/verify_monitor.sh
```

**契约 / 验收**：
```bash
curl -s localhost:8428/api/v1/query?query=up | jq '.data.result | length'  # 期望 ≥ 4
curl -s localhost:8080/api/v1/alerts | jq '.data.items | length'          # 期望 ≥ 1（含时钟/5xx）
# Grafana 4 张看板可访问；vSphere exporter 在线
```

**AI 陷阱**：
- `up` 指标 < 节点数说明有 Agent 没暴露 /metrics，逐个排查
- 验收脚本要可重复跑，别留副作用
