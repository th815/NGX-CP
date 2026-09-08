-- NGXCP 日志存储 schema（ClickHouse 单实例，限内存 + TTL 7 天）
-- 控制面 ApplySchema 会自动建表（CREATE TABLE IF NOT EXISTS）；
-- 本文件供 docker initdb / 人工复核使用。
--
-- 资源约束（见 docs/DECISIONS.md §3/§10、AGENTS.md）：
--   - 本地 128G/25T 极宽裕，但仍设 max_memory_usage 防失控（默认吃 90% 系统内存）。
--   - TTL 7 天：写多读少，仅保留近 7 天明细，聚合/安全事件另存。

-- 连接级内存上限（6G）。也可在 config.yaml 的 clickhouse_max_memory_usage 配置，
-- 控制面建立连接时经 Settings 注入，无需改服务端配置。
SET max_memory_usage = 6442450944;  -- 6 * 1024^3

CREATE TABLE IF NOT EXISTS nginx_access (
    ts              DateTime,
    node            String,
    rid             String,        -- $request_id（TraceID）
    remote_addr     String,
    server          String,
    uri             String,
    status          UInt16,
    upstream_addr   String,        -- 落在哪个后端（瓶颈定位）
    upstream_status String,
    upstream_rt     Float32,       -- 慢在后端还是 Nginx
    request_rt      Float32,
    bytes           UInt32,
    ua              String,
    raw             String         -- 原始 JSON 行（证据/回放）
) ENGINE = MergeTree
ORDER BY (ts, node)
TTL ts + INTERVAL 7 DAY;

CREATE TABLE IF NOT EXISTS nginx_error (
    ts      DateTime,
    node    String,
    level   String,
    pid     String,
    tid     String,
    message String,
    raw     String
) ENGINE = MergeTree
ORDER BY (ts, node)
TTL ts + INTERVAL 7 DAY;
