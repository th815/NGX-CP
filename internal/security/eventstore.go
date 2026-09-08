package security

import (
	"context"
	"time"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/securityevent"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// Event 是处置状态机对外暴露的安全事件视图（与 ent.SecurityEvent 对齐）。
type Event struct {
	ID        int       `json:"id"`
	RuleID    string    `json:"rule_id"`
	RuleName  string    `json:"rule_name"`
	Level     string    `json:"level"`   // INFO | WARN | CRITICAL
	Node      string    `json:"node"`    // 命中最相关日志的节点标识
	Sample    string    `json:"sample"`  // 证据：原始日志片段
	Handled   bool      `json:"handled"` // 处置锁
	Action    string    `json:"action"`  // pending | blocked | ignored
	CreatedAt time.Time `json:"created_at"`
}

// ListFilter 是事件列表的筛选+分页条件。
type ListFilter struct {
	Level   string // 可选：INFO | WARN | CRITICAL
	Handled *bool  // 可选：按处置状态过滤
	Page    int    // 从 1 起，默认 1
	Size    int    // 每页条数，默认 50，上限 200
}

// EventStore 是安全事件的持久化抽象（PG ent 生产、内存测试）。
// 处置状态机在此落地：Create 落 pending 事件，Handle 置 handled 锁。
type EventStore interface {
	Create(ctx context.Context, e Event) (int, error)
	Get(ctx context.Context, id int) (*Event, error)
	List(ctx context.Context, f ListFilter) ([]Event, int, error)
	Handle(ctx context.Context, id int, action string) error
	// HasActive 判断某规则是否已有未处置事件（调度去重，避免每周期刷一堆重复事件）。
	HasActive(ctx context.Context, ruleID string) (bool, error)
}

// EntEventStore 是 EventStore 的生产实现（ent + PostgreSQL）。
type EntEventStore struct {
	client *ent.Client
}

// NewEntEventStore 构造 ent 事件存储。
func NewEntEventStore(c *ent.Client) *EntEventStore {
	return &EntEventStore{client: c}
}

// Create 写入一条 pending 事件，返回自增 ID。
func (s *EntEventStore) Create(ctx context.Context, e Event) (int, error) {
	action := securityevent.ActionPending
	if e.Action != "" {
		action = securityevent.Action(e.Action)
	}
	ev, err := s.client.SecurityEvent.Create().
		SetRuleID(e.RuleID).
		SetRuleName(e.RuleName).
		SetLevel(securityevent.Level(e.Level)).
		SetNode(e.Node).
		SetSample(e.Sample).
		SetAction(action).
		Save(ctx)
	if err != nil {
		return 0, err
	}
	return ev.ID, nil
}

// Get 按 ID 取事件；不存在返回 CodeNotFound。
func (s *EntEventStore) Get(ctx context.Context, id int) (*Event, error) {
	ev, err := s.client.SecurityEvent.Query().Where(securityevent.ID(id)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "事件不存在")
		}
		return nil, err
	}
	return toEvent(ev), nil
}

// List 多维筛选 + 分页（按 created_at 降序，最新在前）。
func (s *EntEventStore) List(ctx context.Context, f ListFilter) ([]Event, int, error) {
	q := s.client.SecurityEvent.Query()
	if f.Level != "" {
		q = q.Where(securityevent.LevelEQ(securityevent.Level(f.Level)))
	}
	if f.Handled != nil {
		q = q.Where(securityevent.Handled(*f.Handled))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
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
	evs, err := q.Order(securityevent.ByCreatedAt()).
		Offset((page - 1) * size).
		Limit(size).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]Event, 0, len(evs))
	for _, ev := range evs {
		out = append(out, *toEvent(ev))
	}
	return out, total, nil
}

// Handle 处置事件：置 handled 锁并落动作；已处置则返回 CodeConflict（防重复动作）。
func (s *EntEventStore) Handle(ctx context.Context, id int, action string) error {
	if action != "blocked" && action != "ignored" {
		return apperr.New(apperr.CodeInvalid, "非法处置动作，仅支持 blocked / ignored")
	}
	ev, err := s.client.SecurityEvent.Query().Where(securityevent.ID(id)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "事件不存在")
		}
		return err
	}
	if ev.Handled {
		return apperr.New(apperr.CodeConflict, "事件已处置，禁止重复动作")
	}
	_, err = ev.Update().SetHandled(true).SetAction(securityevent.Action(action)).Save(ctx)
	return err
}

// HasActive 判断某规则是否已有未处置（handled=false）事件。
func (s *EntEventStore) HasActive(ctx context.Context, ruleID string) (bool, error) {
	return s.client.SecurityEvent.Query().
		Where(securityevent.RuleID(ruleID), securityevent.Handled(false)).
		Exist(ctx)
}

// toEvent 把 ent 实体投影为对外 Event。
func toEvent(ev *ent.SecurityEvent) *Event {
	return &Event{
		ID:        ev.ID,
		RuleID:    ev.RuleID,
		RuleName:  ev.RuleName,
		Level:     string(ev.Level),
		Node:      ev.Node,
		Sample:    ev.Sample,
		Handled:   ev.Handled,
		Action:    string(ev.Action),
		CreatedAt: ev.CreatedAt,
	}
}
