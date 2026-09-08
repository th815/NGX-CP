package logstore

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

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
