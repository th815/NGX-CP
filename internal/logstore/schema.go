package logstore

import (
	"strconv"
	"strings"
)

// SchemaDDL 是 ClickHouse 建表语句（幂等 IF NOT EXISTS）。
// 与 deploy/clickhouse/init.sql 保持一致；控制面 ApplySchema 会用它自动建表。
// TTL 7 天、限内存在生产经连接 Settings 设置（见 clickhouse.go）。
const SchemaDDL = `
CREATE TABLE IF NOT EXISTS nginx_access (
    ts              DateTime,
    node            String,
    rid             String,
    remote_addr     String,
    server          String,
    uri             String,
    status          UInt16,
    upstream_addr   String,
    upstream_status String,
    upstream_rt     Float32,
    request_rt      Float32,
    bytes           UInt32,
    ua              String,
    raw             String
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
`

// ParseMemLimit 把 "6G"/"512M"/"6442450944" 之类人类可读上限解析为字节数。
// 用于 ClickHouse 连接 Settings 的 max_memory_usage（避免默认吃 90% 系统内存）。
func ParseMemLimit(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	var mult int64 = 1
	upper := strings.ToUpper(s)
	switch {
	case strings.HasSuffix(upper, "GB"):
		mult = 1024 * 1024 * 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "MB"):
		mult = 1024 * 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "KB"):
		mult = 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(upper, "G"):
		mult = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(upper, "M"):
		mult = 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(upper, "K"):
		mult = 1024
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return int64(v * float64(mult)), nil
}
