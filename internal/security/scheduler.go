package security

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/th/ngxcp/internal/logstore"
)

// Scheduler 周期跑规则引擎（T066），命中 → 建 SecurityEvent（PG）+ 落库 security_alerts（CH）。
// 与 ctx 同生命周期；首次立即跑一次，之后按 interval 周期执行。
type Scheduler struct {
	engine *Engine
	events EventStore
	alerts AlertStore // 可 nil（无 ClickHouse 时静默跳过落库）
	rules  []Rule
}

// NewScheduler 构造调度器。engine 已内含 Backend（生产为 ClickHouse，无 CH 时为 NoopBackend）。
func NewScheduler(engine *Engine, events EventStore, alerts AlertStore, rules []Rule) *Scheduler {
	return &Scheduler{engine: engine, events: events, alerts: alerts, rules: rules}
}

// Start 启动周期调度。interval<=0 回落 30s。
func (s *Scheduler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	s.runOnce(ctx) // 启动即跑一次，避免空窗
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx)
		}
	}
}

// runOnce 对全部规则评估一次，命中且去重通过则建事件 + 落库。
func (s *Scheduler) runOnce(ctx context.Context) {
	now := time.Now()
	for _, rule := range s.rules {
		hit, err := s.engine.Evaluate(ctx, rule, now)
		if err != nil {
			slog.Default().Warn("安全规则评估失败", "rule", rule.ID, "err", err)
			continue
		}
		if !hit.Triggered {
			continue
		}
		// 去重：该规则已有未处置事件则不重复建（同一持续攻击只建一条 pending，直到被处置）。
		active, aerr := s.events.HasActive(ctx, rule.ID)
		if aerr != nil {
			slog.Default().Warn("查询活跃事件失败", "rule", rule.ID, "err", aerr)
			continue
		}
		if active {
			continue
		}
		ev := Event{
			RuleID:   rule.ID,
			RuleName: rule.Name,
			Level:    hit.Level,
			Node:     nodeOf(hit.Sample),
			Sample:   sampleOf(hit.Sample),
			Action:   "pending",
		}
		if _, cerr := s.events.Create(ctx, ev); cerr != nil {
			slog.Default().Warn("创建安全事件失败", "rule", rule.ID, "err", cerr)
			continue
		}
		if s.alerts != nil {
			_ = s.alerts.RecordAlert(ctx, Alert{
				RuleID:    rule.ID,
				RuleName:  rule.Name,
				Level:     hit.Level,
				Count:     hit.Count,
				Threshold: hit.Threshold,
				Node:      nodeOf(hit.Sample),
				SampleRaw: sampleOf(hit.Sample),
				Ts:        now,
			})
		}
		slog.Default().Info("安全规则命中，已建事件",
			"rule", rule.ID, "level", hit.Level, "count", hit.Count)
	}
}

// nodeOf 取样本节点标识。
func nodeOf(e *logstore.Entry) string {
	if e == nil {
		return ""
	}
	return e.Node
}

// sampleOf 取样本证据：优先原始日志片段，否则退化为整条 JSON 序列化。
func sampleOf(e *logstore.Entry) string {
	if e == nil {
		return ""
	}
	if e.Raw != "" {
		return e.Raw
	}
	b, _ := json.Marshal(e)
	return string(b)
}

// NoopBackend 是生产 fallback：无 ClickHouse 时返回 0（不误报攻击）。
// 满足 security.Backend 接口（与 *logstore.ClickHouseStorage 同构）。
type NoopBackend struct{}

// NewNoopBackend 构造无后端兜底（开发态 / 未接 ClickHouse）。
func NewNoopBackend() NoopBackend { return NoopBackend{} }

// QueryCount 恒返回 0。
func (NoopBackend) QueryCount(_ context.Context, _ string, _ ...any) (float64, error) {
	return 0, nil
}
