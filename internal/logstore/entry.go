// Package logstore 实现 Nginx 日志的落库与攒批。
//
// 设计要点（见 docs/tasks/M6-logs-security.md T062）：
//   - 依赖倒置：Ingester 只认 Storage 接口，生产用 ClickHouseStorage，
//     测试/开发用 MemStorage，故本包可无真机/无 ClickHouse 单测；
//   - 攒批硬约束：单条插入 ClickHouse 会拖垮，必须批量（1000 条 / 5s）；
//   - 资源约束：max_memory_usage 必须设（默认吃 90% 系统内存），TTL 7 天。
//
// 数据来源（上游）：Agent 经 T061 采集 → gRPC 上报控制面 → 转换 Entry →
// Ingester.Accept；传输接线属 T063，本包只提供引擎 + 接口。
package logstore

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Entry 是写入 ClickHouse 的规范化日志记录。
// 字段与 T060 下发的 log_format 键、T061 的 LogLine 对齐。
type Entry struct {
	TS             time.Time
	Node           string
	RID            string
	RemoteAddr     string
	Server         string
	URI            string
	Status         uint16
	UpstreamAddr   string
	UpstreamStatus string
	UpstreamRT     float32
	RequestRT      float32
	Bytes          uint32
	UA             string
	Raw            string
}

// Storage 是日志落库的抽象。生产用 ClickHouseStorage；测试用 MemStorage。
type Storage interface {
	Ingest(ctx context.Context, entries []Entry) error
	Query(ctx context.Context, p QueryParams) (*QueryResult, error)
	Aggregate(ctx context.Context, p AggParams) (*AggResult, error)
	Ping(ctx context.Context) error
	Close() error
}

// MemStorage 内存实现，用于开发与单测（无真实 ClickHouse 依赖）。
type MemStorage struct {
	mu   sync.Mutex
	rows []Entry
	fail bool
}

// NewMemStorage 构造内存存储。
func NewMemStorage() *MemStorage { return &MemStorage{} }

// SetFail 注入失败（测试 flush 失败重缓冲路径）。
func (m *MemStorage) SetFail(v bool) {
	m.mu.Lock()
	m.fail = v
	m.mu.Unlock()
}

// Ingest 追加一批 Entry。
func (m *MemStorage) Ingest(_ context.Context, entries []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("memstorage: injected failure")
	}
	m.rows = append(m.rows, entries...)
	return nil
}

// Len 当前已落库条数。
func (m *MemStorage) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// Rows 返回已落库副本。
func (m *MemStorage) Rows() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, len(m.rows))
	copy(out, m.rows)
	return out
}

// Ping 始终成功（内存实现）。
func (m *MemStorage) Ping(_ context.Context) error { return nil }

// Close 无操作。
func (m *MemStorage) Close() error { return nil }
