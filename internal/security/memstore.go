package security

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

// MemEventStore 是 EventStore 的内存实现，用于开发与单测
// （含 handler 测试复用，验证调度去重与处置锁语义，无需 PG）。
type MemEventStore struct {
	mu   sync.Mutex
	seq  int
	rows []Event
}

// NewMemEventStore 构造内存事件存储。
func NewMemEventStore() *MemEventStore {
	return &MemEventStore{}
}

// Create 追加一条 pending 事件，返回自增 ID。
func (m *MemEventStore) Create(_ context.Context, e Event) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	e.ID = m.seq
	if e.Action == "" {
		e.Action = "pending"
	}
	e.CreatedAt = time.Now()
	m.rows = append(m.rows, e)
	return e.ID, nil
}

// Get 按 ID 取事件；不存在返回 CodeNotFound。
func (m *MemEventStore) Get(_ context.Context, id int) (*Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].ID == id {
			ev := m.rows[i]
			return &ev, nil
		}
	}
	return nil, apperr.New(apperr.CodeNotFound, "事件不存在")
}

// List 多维筛选 + 分页（按 created_at 降序）。
func (m *MemEventStore) List(_ context.Context, f ListFilter) ([]Event, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.rows {
		if f.Level != "" && e.Level != f.Level {
			continue
		}
		if f.Handled != nil && e.Handled != *f.Handled {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	total := len(out)
	page, size := f.Page, f.Size
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 50
	}
	if size > 200 {
		size = 200
	}
	start := (page - 1) * size
	if start > len(out) {
		start = len(out)
	}
	end := start + size
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], total, nil
}

// Handle 处置事件；已处置返回 CodeConflict，非法动作返回 CodeInvalid。
func (m *MemEventStore) Handle(_ context.Context, id int, action string) error {
	if action != "blocked" && action != "ignored" {
		return apperr.New(apperr.CodeInvalid, "非法处置动作，仅支持 blocked / ignored")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].ID == id {
			if m.rows[i].Handled {
				return apperr.New(apperr.CodeConflict, "事件已处置，禁止重复动作")
			}
			m.rows[i].Handled = true
			m.rows[i].Action = action
			return nil
		}
	}
	return apperr.New(apperr.CodeNotFound, "事件不存在")
}

// HasActive 判断该规则是否已有未处置事件。
func (m *MemEventStore) HasActive(_ context.Context, ruleID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.rows {
		if e.RuleID == ruleID && !e.Handled {
			return true, nil
		}
	}
	return false, nil
}
