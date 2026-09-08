package security

import (
	"context"
	"time"

	"github.com/th/ngxcp/internal/logstore"
)

// AlertStore 把规则命中落库到 ClickHouse 的 security_alerts 表（T066 建表）。
// 与 PG 的 SecurityEvent（处置状态机）分层：CH 管命中原始时序，PG 管处置。
type AlertStore interface {
	RecordAlert(ctx context.Context, a Alert) error
}

// Alert 是落库的安全告警记录（字段与 security_alerts 表对齐）。
type Alert struct {
	RuleID    string
	RuleName  string
	Level     string
	Node      string
	SampleRaw string
	Count     float64
	Threshold float64
	Ts        time.Time
}

// CHAlertStore 经 logstore.ClickHouseStorage.Exec 落库。
// security 包持有 *logstore.ClickHouseStorage 不构成循环依赖（logstore 不 import security）。
type CHAlertStore struct {
	ch *logstore.ClickHouseStorage
}

// NewCHAlertStore 构造 ClickHouse 告警落库器。
func NewCHAlertStore(ch *logstore.ClickHouseStorage) *CHAlertStore {
	return &CHAlertStore{ch: ch}
}

// RecordAlert 参数化 INSERT（? 占位，杜绝注入）。
func (s *CHAlertStore) RecordAlert(ctx context.Context, a Alert) error {
	return s.ch.Exec(ctx,
		`INSERT INTO security_alerts (ts, rule_id, rule_name, level, count, threshold, node, sample_raw)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Ts, a.RuleID, a.RuleName, a.Level, a.Count, a.Threshold, a.Node, a.SampleRaw,
	)
}
