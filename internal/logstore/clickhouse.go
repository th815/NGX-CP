package logstore

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ClickHouseStorage 是生产用 Storage：经 clickhouse-go/v2 批量写入。
// 攒批用驱动的 PrepareBatch + Append + Send（T062 硬约束：单条插入会拖垮）。
type ClickHouseStorage struct {
	conn    driver.Conn
	ttlDays int
	log     *slog.Logger
}

// NewClickHouse 从 DSN（如 clickhouse://127.0.0.1:9000/ngxcp）建立连接。
// maxMemBytes>0 时设置连接级 max_memory_usage（默认吃 90% 系统内存，必须限）。
// ttlDays<=0 回落 7 天（仅文档/注释意义；TTL 已写入建表 DDL）。
func NewClickHouse(dsn string, maxMemBytes int64, ttlDays int) (*ClickHouseStorage, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if maxMemBytes > 0 {
		if opts.Settings == nil {
			opts.Settings = clickhouse.Settings{}
		}
		opts.Settings["max_memory_usage"] = strconv.FormatInt(maxMemBytes, 10)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, err
	}
	if ttlDays <= 0 {
		ttlDays = 7
	}
	return &ClickHouseStorage{conn: conn, ttlDays: ttlDays, log: slog.Default()}, nil
}

// ApplySchema 幂等建表（CREATE TABLE IF NOT EXISTS）。
func (s *ClickHouseStorage) ApplySchema(ctx context.Context) error {
	for _, stmt := range splitDDL(SchemaDDL) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if err := s.conn.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// Ingest 批量写入 nginx_access。空批直接返回。
func (s *ClickHouseStorage) Ingest(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx, "INSERT INTO nginx_access")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := batch.Append(
			e.TS, e.Node, e.RID, e.RemoteAddr, e.Server, e.URI,
			e.Status, e.UpstreamAddr, e.UpstreamStatus,
			e.UpstreamRT, e.RequestRT, e.Bytes, e.UA, e.Raw,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

// Ping 探活。
func (s *ClickHouseStorage) Ping(ctx context.Context) error { return s.conn.Ping(ctx) }

// Close 关闭连接。
func (s *ClickHouseStorage) Close() error { return s.conn.Close() }

// Query 执行多维检索（T063）。WHERE 与参数由 buildWhere 统一构造，全部走 ? 占位，
// 杜绝 SQL 注入；先取分页数据，再取满足条件的总数。
func (s *ClickHouseStorage) Query(ctx context.Context, p QueryParams) (*QueryResult, error) {
	p = p.normalize()
	start := time.Now()

	dataQ, dataArgs := buildDataQuery(p)
	rows, err := s.conn.Query(ctx, dataQ, dataArgs...)
	if err != nil {
		return nil, err
	}
	items := make([]Entry, 0, p.Size)
	for rows.Next() {
		var e Entry
		if err := rows.Scan(
			&e.TS, &e.Node, &e.RID, &e.RemoteAddr, &e.Server, &e.URI,
			&e.Status, &e.UpstreamAddr, &e.UpstreamStatus,
			&e.UpstreamRT, &e.RequestRT, &e.Bytes, &e.UA, &e.Raw,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, e)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	countQ, countArgs := buildCountQuery(p)
	var total int64
	if err := s.conn.QueryRow(ctx, countQ, countArgs...).Scan(&total); err != nil {
		return nil, err
	}

	return &QueryResult{Items: items, Total: total, TookMs: time.Since(start).Milliseconds()}, nil
}

// splitDDL 按 ';' 切分建表语句（DDL 内不含字符串字面量分号，安全）。
func splitDDL(ddl string) []string {
	parts := strings.Split(ddl, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// QueryCount 参数化执行查询并返回 float64（供 internal/security 规则引擎后端）。
// 规则 SQL 一律返回 toFloat64(...)，故 Scan 到 float64 安全。
func (s *ClickHouseStorage) QueryCount(ctx context.Context, query string, args ...any) (float64, error) {
	var v float64
	if err := s.conn.QueryRow(ctx, query, args...).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// Exec 执行任意参数化语句（如 security_alerts 落库 INSERT）。
func (s *ClickHouseStorage) Exec(ctx context.Context, query string, args ...any) error {
	return s.conn.Exec(ctx, query, args...)
}
