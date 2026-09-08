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

> **状态（2026-09-08）**：已完成 `internal/domain/logfmt/`（format.go 渲染 + plan.go 前置检查 + service.go 下发编排）、`internal/pkg/nginxconf/scope.go`（块作用域扫描）、`internal/server/handler/logformat.go` + `router.go` 三个端点。
>
> **对上方契约草案做了三处必要修正（草案照抄会炸线上，勿回退）**：
> ① **不覆写 `/var/log/nginx/access.log`**，改为独立片段文件 `<include_dir>/zz-ngxcp-logformat.conf` + **额外一条** `access_log /var/log/nginx/ngxcp-access.json.log ngxcp_json buffer=32k flush=5s;`。nginx 同层级多条 access_log 并行写入，存量文本日志与格式原样不动，业务零感知（符合「只增不改」约束）；代价是双写占盘，Plan 恒发 logrotate 告警。
> ② **所有 JSON 值一律加引号**。草案的 `"upstream_rt":$upstream_response_time` 是错的——静态文件/redirect/error_page 无 upstream 时该变量为空串，渲染出 `"upstream_rt":,` 属非法 JSON，整行报废。加引号后恒合法，代价是数值变字符串，故 T061 采集侧同批落地 `flexInt/flexUint32/flexFloat32`（`internal/agent/logtail/flexnum.go`）容忍带引号数字、空串、`"-"`、重试逗号列表（取首项），脏值退化为 0 而非杀掉整行。
> ③ 格式名用 `ngxcp_json`（非 `json_main`），文件名 `zz-` 前缀保证在 include 目录内最后加载。
>
> **下发路径复用 M3 变更单流水线**（不新开通道）：`EnsureFile` → `CreateRevision(source=log_format)` → `deploy.CreateDraft(serial + 观测 60s + 自动回滚)`，因此天然可灰度/回滚/审批；`apply` 只到 draft，仍需 `/change-orders/:id/submit` 才真正发布。ent `config_revision.source` 枚举新增 `log_format`（已 `go generate`）。
>
> **三类可预判失败拦在预览阶段**（不留给 `nginx -t`）：nginx < 1.11.8 不支持 `escape=json`；http{} 内无通配 include（最隐蔽——发布"成功"但片段永不加载，平台无日志）；server/location 层级已有 access_log 会就近覆盖（那些站点静默缺 JSON 日志，预览列出待单独下发）。平台**不改写主配置**，缺 include 时给人工修复指引后拒绝下发。
>
> **API**：`GET /api/v1/logs/format`（契约自述，只读免鉴权）、`POST /api/v1/logs/format/preview`（逐节点计划+告警+`blocked` 列表，鉴权）、`POST /api/v1/logs/format/apply`（鉴权）。preview 逐节点列举失败原因，apply 则任一节点不可行即整体失败——不允许集群内两台 RS 一台有 JSON 日志一台没有，那会让跨节点追踪结果似真而假。
>
> **测试**：`nginxconf/scope_test.go` 4 例、`logfmt/format_test.go` 6 例（含反射比对 `logtail.LogLine` json tag 锁死字段契约、渲染结果回灌 Agent 解析、空 upstream 场景）、`logtail/flexnum_test.go` 6 例、`handler/logformat_test.go` 5 例，`testdata/access_log_sample.jsonl` 7 行样例回归。`go build`/`go vet`/`go test ./...` 全过。
>
> **未完成/未验证**：① 未接前端 UI（M6 前端整体待做，`web/src/views` 尚无 Logs 页面）；② **未真机验证**——验收命令 `tail -1 /var/log/nginx/ngxcp-access.json.log | jq -e '.rid and .upstream_addr and .upstream_rt'` 需在节点应用并 reload 后执行，注意无 upstream 的请求该断言会为空（属预期，应挑一条经过 proxy_pass 的请求验证）。

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

> **状态（2026-09-08）**：已完成 `internal/agent/logtail/`（line.go/offset.go/queue.go/tail.go + tail_test.go）。`Tailer.Run(ctx, emit)` 从 offset 续读、inode 变化应对 logrotate、同 inode 截断重置、按 SampleRate 降采样（4xx/5xx 与无法解析行恒保留）、攒批调用注入式 emit；emit 失败进磁盘队列 store-forward（JSONL、保留 24h、启动回放），offset 原子写。六类单测全过。**T063-补 已接线**：`batch.go` 加 `MarshalBatch/UnmarshalBatch`，`runtime.go` 经 `CollectLogTargets` 取目标起 `Tailer` 并把 `[]LogLine` 打包经心跳流 `LOG_BATCH` 上行控制面 → `Ingester.Accept`，端到端可验证（见 T063 补做状态）。**T061-补（2026-09-08）用户实测反馈**：存量 Nginx 业务日志是**标准文本格式（非 JSON）**，原 `ParseLine` 仅解 JSON 致真机读不出。已补标准 `combined` 文本正则（`combinedRe`+`trailingRe` 匹配 `request_time/upstream_addr/upstream_status/upstream_rt/request_id` 后缀），`ParseLine` 改 JSON/文本自动探测（文本行不可能以 `{` 开头，探测安全），`Tailer.Format` 透传 capability `LogTarget.Format`（`""`=combined 存量/`"json"`=T060 标准/未知回退自动探测）。关键修复：combined 用 `$time_local`(CLF)，解析后转 RFC3339 存 `TS` 否则控制面 `DefaultParseTS` 失败→全变 1970，`convert_test.go` 端到端锁。各用户自定义非标准 log_format 仍可能匹配不上（T060 下发 JSON 彻底解决）。

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

> **状态（2026-09-08）**：已完成 `internal/logstore/`（entry.go/convert.go/schema.go/ingester.go/clickhouse.go + clickhouse_test.go）与 `deploy/clickhouse/init.sql`。`Entry` 规范化记录（字段对齐 T060 日志格式与 T061 LogLine）；`Storage` 接口依赖倒置，`ClickHouseStorage` 经 `clickhouse-go/v2`(`PrepareBatch+Append+Send`) 批量写入、连接级 `max_memory_usage` 由 `ParseMemLimit("6G")` 设 6G、`ApplySchema` 幂等建表；`MemStorage` 供测试。`Ingester` 攒批（batchSize 1000 / flushEvery 5s，达量+周期+Close 三路径 flush，入库失败重缓冲）。`go build`/`go vet`/`go test ./internal/logstore/...` 七类单测全过。**T063-补 已消费**：`server.go` 建 `Ingester` 并经 `agentSrv.SetLogIngester` 接 Agent `LOG_BATCH` 上报，端到端可验证。生产 ClickHouse 实例与真机写入未验证。

---

## T063 · 日志检索 API（完成，2026-09-08）

> **状态（2026-09-08）**：已完成 `internal/logstore/query.go`（QueryParams/QueryResult/normalize/buildWhere 参数化防注入/escapeLike/MemStorage.Query 内存过滤+分页）+ `internal/server/handler/logs.go`（`POST /api/v1/logs/search`，多维筛选 status/nodes/uri/ip/rid/rt_min + regex 开关 + 分页，返回 `{code,data:{items,total,took_ms}}`）+ `router.go` 路由 + `server.go` 按 `NGXCP_LOGSTORE_DSN` 构造 `ClickHouseStorage`（缺省 `MemStorage`）。`Storage` 接口新增 `Query`；`ClickHouseStorage.Query` 复用 `buildWhere` 走参数化 SQL + 计数查询。`query_test.go`（参数化/escapeLike/MemStorage 多维过滤+分页+默认时间窗）+ `logs_test.go`（httptest 端点）共 11 类单测全过。
>
> **补做（2026-09-08）· 日志传输接线**：把 T061→T063 闭合成端到端管道。离线 patch `gen/agent/v1/agent.pb.go` `rawDesc`：`HeartbeatRequest.Type` 加 `LOG_BATCH=12`、`HeartbeatRequest` 加字段 14 `log_batch`（`bytes`，沿用 T056 bytes 同构手法避免牵动消息注册表）；`rawdesc_logbatch_test.go` 锁往返。`internal/agent/logtail/batch.go` 加 `MarshalBatch/UnmarshalBatch`。Agent 侧：`heartbeat.go` 加 `StartLogTail` 回调 + `logBatchOut` goroutine 周期上行 `LOG_BATCH`；`runtime.go` 注入 `startLogTail`（用 `CollectLogTargets` 取非 off/variable/syslog 目标起 `Tailer`）。控制面侧：`grpc_server.go` 加 `LogAcceptor` 接口 + `SetLogIngester`，`Heartbeat` 消费 `req.GetLogBatch()`→`FromLogLines`→`Accept`；`server.go` 建 `Ingester` 并 `SetLogIngester`。`heartbeat_test.go` 加 `TestHeartbeaterReportsLogBatch`、`grpc_server_test.go` 加 `TestHeartbeatLogBatchIngested`/`TestHeartbeatLogBatchEndToEnd`（Ingester→MemStorage→Query(status=503) 命中）。**生产 ClickHouse 实例与真机写入未验证**；检索现已对 Agent 上报数据生效（受 `NGXCP_LOGSTORE_DSN` 控制，缺省 `MemStorage`）。

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

> **状态（2026-09-08）**：已完成 `internal/server/handler/logs.go` 的 `Trace`（`GET /api/v1/logs/trace/:request_id`，复用 T063 `Storage.Query(RID)` 取该 rid 全部 span，按 ts 升序还原链路，计算 `nodes`/`first_hop`(最早 span 节点)/`bottleneck`(upstream_rt 最大节点)）+ `router.go` 路由注册。`logs_test.go` 加 `TestLogsHandler_Trace`（跨 2 节点升序/节点集合/首跳/瓶颈 + 不存在 rid 空结果）。`go build`/`go vet`/`go test ./internal/server/handler/...` 全过。**未真机验证**：依赖 T060 把 `$request_id` 写进节点日志格式，节点未应用标准格式时追踪为空（同 T061 缺真实数据来源）。

> **状态（2026-09-08）**：已完成 `internal/logstore/aggregate.go`（AggMetric/AggParams/AggRow/AggResult + 纯函数 `computeAgg`：top_uri/top_ip/top_ua 计数+错误数 TopN、status_dist 状态码分布、rt_percentile 插值法 P50/P95/P99 + `parseWindow`("1h/24h/7d"，Go 标准库不支持 "d" 故单独处理) + `aggregateFromQuery`）+ `Storage` 接口加 `Aggregate`（`MemStorage`/`ClickHouseStorage` 均经 Query 取行后调 `computeAgg`，刻意不引入未经真机验证的 CH GROUP BY SQL）。`handler/logs.go` 加 `Aggregate`（`POST /api/v1/logs/aggregate`，body 绑定 metric/window/nodes/status/uri/ip/rid/rt_min/regex/top_n`）+ `router.go` 注册；`aggregate_test.go`（top_uri/status_dist/rt_percentile/topN 上限/空数据/parseWindow）+ `logs_test.go`（`TestLogsHandler_Aggregate` 三类指标 + 非法指标 400）共覆盖。`go build`/`go vet`/`go test ./internal/logstore/... ./internal/server/handler/...` 全过。生产 ClickHouse 与真机写入未验证；聚合准确性已通过 MemStorage 单测验证。

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

> **状态（2026-09-08）**：已完成 `internal/security/rules.go`（Rule 结构体 / `DefaultRules()` 10 条 / `Engine` + `EvaluateMem` 内存评估 + `CHBackend` 参数化后端）+ `internal/security/rules_test.go`（12 例）+ `deploy/clickhouse/security.sql`（security_alerts 落库表，TTL 30d；与 PG 的 security_event ent schema 分层——CH 管命中时序、PG 管处置状态机）。
>
> **双轨可验设计（关键，防「规则即 SQL」变黑盒）**：生产走 ClickHouse，每条规则是一条 `SELECT toFloat64(count()) FROM nginx_access WHERE ts BETWEEN ? AND ? AND <特征>` 滑动窗口查询，窗口起止用 `?` 占位参数化绑定、阈值只在 Go 侧 `count >= Threshold` 比较——绝不拼接用户输入防注入；沙箱/测试/MemStorage 模式走 `EvaluateMem(rule, entries, now)`，用同一语义的 `Rule.Eval` 内存统计。两条路径对同一组构造样本必须一致：测试 `TestRules_EvalMatchesSQLIntent` 用表面特征哨兵（如注入规则 SQL 必须含 `union select`、爆破规则必须含 `status = 404` + `count(distinct uri)`）锁死 Eval 与 SQL 不漂移。
>
> **10 条规则与动作分级**（误报面大的默认 `alert`，仅「单 IP 高频 CC」这种极高置信才默认 `auto`，符合 T069 纪律）：
> | ID | 名称 | Level | Action | 窗口 | 阈值(初值) | 检测语义 |
> |---|---|---|---|---|---|---|
> | r-sql-injection | SQL 注入特征 | CRITICAL | semi | 5m | 1 | uri/ua 含 union select / or 1=1 / select from / `' or '` / `-- ` / `/**/` / `<script` |
> | r-scanner-ua | 扫描器指纹 UA | INFO | alert | 5m | 5 | ua 命中 sqlmap/nmap/nuclei/gobuster/... 列表 |
> | r-dir-brute | 目录爆破 | WARN | semi | 5m | 30 | 单 IP 在窗口内尝试的不同 404 路径数 |
> | r-cc-flood | CC 洪水 | CRITICAL | **auto** | 1m | 600 | 单 IP 在窗口内总请求数（max over group） |
> | r-slowloris | Slowloris 慢速 | WARN | semi | 5m | 10 | request_rt>10s 且 bytes<1024 |
> | r-5xx-spike | 5xx 突增 | WARN | alert | 5m | 100 | 窗口内 5xx 总数（可能是后端故障非攻击） |
> | r-4xx-spike | 4xx 突增 | INFO | alert | 5m | 500 | 窗口内 4xx 总数 |
> | r-sensitive-path | 敏感路径探测 | WARN | alert | 5m | 5 | uri 命中 /wp-admin /.env /phpmyadmin /etc/passwd /... |
> | r-odd-ua | 非常规 UA | INFO | alert | 5m | 50 | ua 为空/`-`/命中 curl/wget/python/go-http-client/... |
> | r-single-ip-broad | 单 IP 高频遍历 | WARN | alert | 5m | 200 | 单 IP 在窗口内访问的不同 URI 数（max over group） |
>
> **阈值初值依据**：自用规模 2 RS、百万级日访问，单 IP 1 分钟 600 请求已明显异常（CC），爆破 30 个不同 404 路径/5m 是扫描特征，遍历 200 个不同 URI/5m 偏激进故仅 alert。均为可调初值，上线后按真实流量在 T067 观测误报/漏报微调。
>
> **测试**：`TestDefaultRules_Structure`(10 条字段合法/SQL 含 ≥2 个 `?`/Eval 非 nil) + `TestParseWindow` + `TestSQLInjection_TriggersCRITICAL`(注入样本→CRITICAL+sample) + `TestCCFlood_Auto`(700 条→auto，70 条→不触发) + `TestDirBrute_Triggers`(单 IP 50 个 404 路径→触发，50 IP 各 1 个→不触发) + `TestSingleIPBroad_Triggers`(220 个不同 URI→触发) + `TestFalseNegative_NormalTraffic`(正常流量不触发 critical/auto) + `TestEngine_EvaluateViaBackend`(FakeBackend 超/低阈值、后端报错上抛) + `TestRules_SQLUsesParameters`(防拼接) + `TestRules_EvalMatchesSQLIntent`(双轨不漂移)。`go build`/`go vet`/`go test ./...` 全过。
>
> **未真机验证**：SQL 在真 ClickHouse 上的执行结果未验（沙箱无 CH 实例，仅验证了语法层面 `positionCaseInsensitive`/`coalesce(max())` 等函数用法合理）；阈值需按真实流量调参。规则引擎本身（含 EvalMem 语义、Engine 后端抽象）已通过测试验证。

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

> **状态（2026-09-08）**：已完成「检测→事件→处置」闭环。
>
> **相对上方草案的字段偏离（必要，勿回退）**：草案 `SecurityEvent` 用 `NodeID int` / `RuleID int`，但实际落库为 `rule_id string` + `node string`——原因：① T066 规则 ID 是字符串（`r-sql-injection` 等），不是 ent 自增 int；② 命中来自日志的 `node` 字段（节点标识字符串），不是 ent 节点自增 ID。故 `ent/schema/security_event.go` 收敛为 `rule_id`(string)/`rule_name`(string)/`level`(Enum INFO|WARN|CRITICAL)/`node`(Optional string)/`sample`(Optional Text 证据)/`handled`(Bool 默认 false)/`action`(Enum pending|blocked|ignored 默认 pending)/`created_at`(Time 默认 now, Immutable)，索引 handled/level/rule_id。`go generate ./ent` 生成 `ent/securityevent/`（常量 `LevelINFO/LevelWARN/LevelCRITICAL`、`ActionPending/ActionBlocked/ActionIgnored`、谓词 `ID/RuleID/Handled/LevelEQ/ActionEQ`）。
>
> **数据分层（T066 已定）**：ClickHouse `security_alerts`（TTL 30d）管命中时序；PG `security_event` ent 管处置状态机。二者经 `SecurityEvent.Handle` 处置锁关联。
>
> **涉及文件**：
> ```
> ent/schema/security_event.go                     # 处置状态机 schema + go generate
> internal/security/eventstore.go                  # EntEventStore：Create/Get/List/Handle/HasActive
> internal/security/alertstore.go                  # CHAlertStore：RecordAlert 参数化 INSERT → security_alerts
> internal/security/scheduler.go                   # Scheduler：周期跑规则→命中→去重建事件+落库；NoopBackend 兜底
> internal/security/memstore.go                    # MemEventStore：测试用（验证去重/处置锁）
> internal/logstore/clickhouse.go                  # 加 QueryCount/Exec 供 security 复用（*ClickHouseStorage 满足 security.Backend）
> internal/server/handler/security.go             # GET /security/events、GET /security/events/:id、POST /security/events/:id/handle(挂 auth)
> internal/server/router.go / server.go           # 接线：有 CH 用 CH 后端+落库，无 CH 回落 NoopBackend
> ```
>
> **调度去重（关键，防刷屏）**：`Scheduler.runOnce` 逐规则 `engine.Evaluate` → 命中且 `HasActive(ruleID)` 为 false 才 `Create` 事件 + `RecordAlert`；同一持续攻击只建一条 pending，直到被处置。无 ClickHouse 时 `NoopBackend.QueryCount` 恒返回 0（不误报、不阻断启动），且无 CH 时 `alerts` 为 nil 静默跳过落库。调度与 ctx 同生命周期、启动即跑一次（`go secSched.Start(ctx, 30*time.Second)`，沿用 T045 调度范式）。
>
> **处置锁**：`Handle(id, action)` 仅接受 `blocked`/`ignored`；已处置返回 `CodeConflict`(409) 防重复动作、非法动作返回 `CodeInvalid`(400)。`MemEventStore`/`EntEventStore` 行为一致（`handler/security_test.go` 锁定）。
>
> **API**：`GET /api/v1/security/events?level=&handled=&page=&size=`（多维筛选+分页，返回 `{code,data:{items,total}}`）、`GET /api/v1/security/events/:id`、`POST /api/v1/security/events/:id/handle`（写操作，router 挂 `RequireAuth` 中间件）。
>
> **测试**：`handler/security_test.go`（`TestSecurityHandler_List` 筛选 level+handled、`TestSecurityHandler_GetNotFound` 404、`TestSecurityHandler_HandleFlow` 成功→200/重复→409/非法→400、`TestSecurityHandler_SchedulerDedup` 多周期仅每规则一条 pending）共 4 例。`go build`/`go vet`/`go test ./...` 全过（security 12 + handler 4）。
>
> **未真机验证**：PG ent 行为、ClickHouse `security_alerts` 落库、调度在真机周期运行，均仅经单测 + 编译验证（沙箱无 PG/CH 实例）；阈值调参延续 T066 声明（上线后观测微调）。

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
