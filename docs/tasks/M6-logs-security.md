# M6 · 日志与安全（W9–W10）★ 用户痛点模块

> **目标**：解决"用户访问分散到任意 Nginx 节点 → 统一日志管理/筛选排查 + 攻击预警"的核心痛点。
> **完成标志**：输入 `request_id` 查到全链路（跨 2 节点）；注入特征触发封禁且可一键回滚。
>
> 决策依据：`docs/DECISIONS.md` §3（日志统一 + 攻击预警）、§10（时序/ClickHouse）。
> 关键设计：**TraceID 透传 + 标准 JSON 格式 + Agent 内置采集 + ClickHouse（检测规则即 SQL）**；封禁**走发布流水线**，不为写线上新开第二条通道。

---

## T060 · 标准 JSON log_format 下发

**目标**：平台一键把统一 `log_format` 应用到所有节点，保证下游可解析。

**依赖**：T021, M1

**涉及文件**：
```
internal/server/handler/config.go   # 新增"应用标准日志格式"动作
testdata/access_log_sample.jsonl
```

**契约**（下发到节点的 conf.d/zz_logformat.conf）：
```nginx
log_format json_main escape=json '{'
  '"time":"$time_iso8601",'
  '"rid":"$request_id",'           # ★ TraceID
  '"remote_addr":"$remote_addr",'
  '"server":"$server_name",'
  '"uri":"$request_uri",'
  '"status":$status,'
  '"upstream_addr":"$upstream_addr",'        # ★ 落在哪个后端
  '"upstream_status":"$upstream_status",'
  '"upstream_rt":$upstream_response_time,'   # ★ 慢在后端还是 Nginx
  '"request_rt":$request_time,'
  '"bytes":$body_bytes_sent,
  '"ua":"$http_user_agent"'}';
access_log /var/log/nginx/access.log json_main;
```

**验收命令**：
```bash
# 应用后产生一条日志，验证是合法 JSON 且含 rid/upstream_addr/upstream_rt
tail -1 /var/log/nginx/access.log | jq -e '.rid and .upstream_addr and .upstream_rt'
# 期望：jq 解析成功且三字段非空
```

**AI 陷阱**：
- `escape=json` 必须加，否则 UA 含特殊字符会破坏 JSON
- 改 `log_format` 后必须 reload 且确认旧日志格式不混流

---

## T061 · Agent 日志采集模块

**目标**：Agent 内置 tail，offset 持久化 + 本地磁盘队列（断连补传）+ 采样降载。

**依赖**：M1, T060

**涉及文件**：
```
internal/agent/logtail/{tail.go,queue.go,offset.go}
internal/agent/logtail/logtail_test.go
```

**契约**：
```go
// 独立 goroutine；监控 inode 变化应对 logrotate；断连时本地队列保留 24h
type LogTail struct {
    Path      string
    Offset    int64        // 持久化到 <path>.offset
    SampleRate float64     // 高负载时降采样
}
func (t *LogTail) Run(ctx, emit func(batch []LogLine) error)
```

**验收命令**：
```bash
go test ./internal/agent/logtail/...
# 期望：模拟 logrotate（inode 变化）后能从新文件续读；断连 10s 后补传
```

**AI 陷阱**：
- 必须监控 inode 变化处理 logrotate，否则轮转后丢日志
- 大流量时降采样，但安全相关（4xx/5xx）样本不全丢
- offset 持久化失败不能丢数据，用原子写

---

## T062 · ClickHouse schema + 批量入库

**目标**：建日志表，攒批异步写入，TTL 7 天，限内存。

**依赖**：T061, T006

**涉及文件**：
```
deploy/clickhouse/init.sql
internal/logstore/clickhouse.go
internal/logstore/clickhouse_test.go
```

**契约**：
```sql
CREATE TABLE nginx_access (
    ts DateTime, node String, rid String, remote_addr String,
    server String, uri String, status UInt16,
    upstream_addr String, upstream_status String,
    upstream_rt Float32, request_rt Float32, bytes UInt32, ua String
) ENGINE = MergeTree ORDER BY (ts, node)
TTL ts + INTERVAL 7 DAY;
```
```go
// 攒批：async_insert，1000 条 / 5s
// 资源：max_memory_usage = 6G（本地 128G 充裕，但仍设上限防失控）
```

**验收命令**：
```bash
docker exec clickhouse clickhouse-client -q "SELECT count() FROM nginx_access"
# 期望：> 0；TTL 后旧数据自动清除
```

**AI 陷阱**：
- **必须攒批**（async_insert 1000/5s），单条插入 ClickHouse 会拖垮
- 必须设 `max_memory_usage`，默认吃 90% 系统内存
- 本地 128G 很宽裕，但 TTL 7 天 + 限内存是好习惯，别因为资源足就关

---

## T063 · 日志检索 API

**目标**：多维筛选（时间/节点/状态码/URI/IP/rid/耗时）+ 保存查询。

**依赖**：T062

**涉及文件**：
```
internal/server/handler/logs.go
docs/api/openapi.yaml             # 补充 /logs/search
internal/logstore/query.go
```

**契约**：
```go
POST /api/v1/logs/search
{ "time_from","time_to","nodes":[],"status":[],"uri","ip","rid","rt_min"
  ,"regex":false,"page":1,"size":50 }
→ { code, data:{ items:[LogLine], total, took_ms } }
GET/POST /api/v1/logs/saved-queries   # 保存常用查询
```

**验收命令**：
```bash
curl -s localhost:8080/api/v1/logs/search -d '{"status":[500],"size":10}' | jq '.data.total'
# 期望：返回 5xx 数量
```

**AI 陷阱**：
- 用户输入的 URI/IP 可能含正则特殊字符，regex=false 时正确转义
- 时间范围必须带索引列裁剪，否则全表扫

---

## T064 · TraceID 全链路追踪

**目标**：按 `request_id` 跨节点聚合一次请求的全部日志。

**依赖**：T060, T062

**涉及文件**：
```
internal/server/handler/logs.go   # /logs/trace/:rid
```

**契约**：
```go
GET /api/v1/logs/trace/:request_id
→ { code, data:{ spans:[LogLine ordered by ts], nodes:[...] } }
// 同时返回首跳节点 + 命中的 upstream_addr（瓶颈定位）
```

**验收命令**：
```bash
curl -s localhost:8080/api/v1/logs/trace/8f3c1a9b | jq '.data.spans | length'
# 期望：>= 1，且能看出请求落在哪个 RS、upstream_rt 多少
```

**AI 陷阱**：
- TraceID 依赖 T060 的 `$request_id` 写入日志；若节点没应用格式，追踪为空
- 跨节点聚合要注意**时钟同步**（见 M7/T077），否则顺序错乱

---

## T065 · 聚合分析 API

**目标**：Top URI / Top IP / Top UA / 状态码分布 / P50·P95·P99 耗时。

**依赖**：T062

**涉及文件**：
```
internal/logstore/aggregate.go
internal/logstore/aggregate_test.go
```

**契约**：
```go
POST /api/v1/logs/aggregate
{ "metric":"top_uri|top_ip|top_ua|status_dist|rt_percentile", "window":"1h|24h", ... }
→ { code, data:{ rows:[{key,value,...}] } }
// rt_percentile 用 quantileExactIf
```

**验收命令**：
```bash
curl -s localhost:8080/api/v1/logs/aggregate -d '{"metric":"rt_percentile","window":"24h"}' | jq '.data.rows'
# 期望：返回 p50/p95/p99 数值
```

**AI 陷阱**：
- 用 `quantileExactIf` 而非近似函数，量小且准确
- Top N 要加 `LIMIT`，避免返回海量

---

## T066 · 攻击检测规则引擎

**目标**：滑动窗口统计，10 条规则覆盖主要攻击面。

**依赖**：T062

**涉及文件**：
```
internal/security/rules.go
internal/security/rules_test.go
deploy/clickhouse/security.sql
```

**契约**：
```go
// 规则即 SQL（滑动窗口）
type Rule struct {
    ID, Name string
    Level    string   // INFO | WARN | CRITICAL
    SQL      string   // 查询异常计数
    Window   string   // "5m"
    Threshold float64
    Action   string   // "auto" | "semi" | "alert"
}
// 10 条：SQL 注入特征、扫描器指纹、目录爆破、CC 洪水、Slowloris、
//        5xx 突增、4xx 突增、敏感路径探测、非常规 UA、单 IP 高频
```

**验收命令**：
```bash
go test ./internal/security/... -run Rules
# 期望：注入特征样本触发 CRITICAL 规则
```

**AI 陷阱**：
- 规则是 SQL，**必须用参数化**，防止规则文本注入
- 阈值要可配，别写死；误报率高的规则默认 `alert` 而非 `auto`

---

## T067 · 告警中心

**目标**：安全事件流 + 分级 + 处置状态 + 证据样本。

**依赖**：T066

**涉及文件**：
```
ent/schema/security_event.go
internal/server/handler/security.go
```

**契约**：
```go
type SecurityEvent struct {
    ID, RuleID int
    Level      string   // INFO | WARN | CRITICAL
    NodeID     int
    Sample     string   // 触发时的原始日志样本（证据）
    Handled    bool
    Action     string   // "blocked" | "ignored" | "pending"
    CreatedAt  time.Time
}
```

**验收命令**：
```bash
curl -s "localhost:8080/api/v1/security/events?level=CRITICAL" | jq '.data.items | length'
# 期望：>= 1（注入测试产生的事件）
```

**AI 陷阱**：
- 证据样本要存原始日志片段，方便事后复盘
- 事件一旦处置（封禁/忽略）状态要锁，避免重复动作

---

## T068 · 封禁变更单（复用发布流水线）

**目标**：规则命中 → 生成 `zz-blocklist.conf` 片段 → 走 M3 变更单（校验/灰度/探活/回滚）。

**依赖**：T066, M3(T030)

**涉及文件**：
```
internal/security/block.go
internal/security/block_test.go
```

**契约**：
```go
func BlockIP(ctx, ip string, reason string) (*ChangeOrder, error) {
    frag := fmt.Sprintf("deny %s;\n", ip)   // 默认放 http 块
    return createChangeOrder(type="security_block", fragment=frag,
                             strategy=lvs_graceful, auto_rollback=true)
}
// 解封 DELETE /security/blocklist/:ip 同样走流水线
```

**验收命令**：
```bash
curl -s -X POST localhost:8080/api/v1/security/events/1/block
# 期望：创建一条 security_block 变更单，下发后 deny 生效
# 误伤后点「回滚」→ 片段移除，配置恢复
```

**AI 陷阱**：
- 封禁**绝不**直接改线上配置，必须走变更单（可回滚/可灰度/有审批）
- `deny` 放 `http` 块对全 server 生效；要按 server 粒度需更细片段
- 解封也是变更单，不能旁路

---

## T069 · 分级处置策略

**目标**：每条规则可配 auto / semi / alert。

**依赖**：T066, T068

**涉及文件**：
```
internal/security/policy.go
```

**契约**：
```go
// auto  : 高置信直接执行封禁（走 T068，但免审批）
// semi  : 创建变更单等审批
// alert : 只记录事件，人工处置
func ApplyPolicy(rule Rule, evt SecurityEvent) error
```

**验收命令**：
```bash
go test ./internal/security/... -run Policy
# 期望：auto 规则直接封禁；alert 规则只留事件
```

**AI 陷阱**：
- auto 规则误伤面大，默认只对极高置信（如明确注入 payload）开放
- 任何 auto 动作都要有对应回滚路径

---

## T070 · 日志中心 + 安全预警 UI

**目标**：检索表单 + 结果表 + TraceID 追踪 + 聚合视图 + 告警规则 + 封禁联动。

**依赖**：T060–T069

**涉及文件**：
```
web/src/views/logs/{Search,Aggregate}.vue
web/src/views/security/{Alerts,Rules}.vue
web/src/components/logs/TracePanel.vue
```

**要点**：
- 检索页：多维筛选 + 正则开关 + 结果表（状态色块）+ 行内「查看原始 JSON」
- 输入 request_id → TracePanel 展示全链路 span 时间线，标出瓶颈 RS
- 安全页：CRITICAL 事件红标 + 「封禁」按钮（触发 T068 变更单）+ 处置状态机
- 规则页：10 条规则列表，阈值/动作可编辑

**验收命令**：
```bash
cd web && npm run build && npm run typecheck
# 期望：构建通过；点封禁触发变更单并出现在发布列表
```

**AI 陷阱**：
- TracePanel 时间线排序依赖节点时钟同步（M7/T077），前端要标注「若时间乱序请检查 NTP」
- 原始 JSON 展示注意 XSS，用 `<pre>` 文本而非 v-html
