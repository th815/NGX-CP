-- NGXCP 安全检测落库 schema（ClickHouse 单实例，限内存 + TTL 30 天）
-- 配合 internal/security 规则引擎（M6 T066/T067）。
--
-- 数据分层（见 docs/DECISIONS.md §3/§10）：
--   - nginx_access / nginx_error：原始日志明细（TTL 7 天），规则引擎在此上跑 SQL。
--   - security_alerts（本文件）：规则命中结果的时序落库（TTL 30 天），
--     供命中趋势/历史回溯；与 PG 的 security_event（T067 ent schema）互补——
--     PG 管「处置状态机」（pending/blocked/ignored），CH 管「命中原始时序」。
--   - 控制面 ApplySecuritySchema 会建表（CREATE TABLE IF NOT EXISTS）；
--     本文件供 docker initdb / 人工复核。

CREATE TABLE IF NOT EXISTS security_alerts (
    ts          DateTime,
    rule_id     String,
    rule_name   String,
    level       String,        -- INFO | WARN | CRITICAL
    count       Float64,       -- 触发时的异常计数
    threshold   Float64,
    node        String,        -- 命中最激进样本所在节点（若有）
    sample_raw  String         -- 首个命中日志样本（证据，原始 JSON）
) ENGINE = MergeTree
ORDER BY (ts, rule_id)
TTL ts + INTERVAL 30 DAY;
