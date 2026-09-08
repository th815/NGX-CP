// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package cert 的 ACME 自动续期调度器（M4 T045）。
//
// 到期前 horizon（默认 30 天）起，RenewDue 遍历待续期证书并执行续期；
// 同一证书连续失败达到阈值（默认 3）触发 onCritical（升级 CRITICAL 告警）。
// 续期成功/失败均经 Recorder 落一条 source=auto_renew / type=cert_renew 变更单作审计。
//
// 设计：调度器只依赖三个小而专注的接口（Renewer / DueLister / Recorder），
// 因此无需真实 ACME/数据库即可单测；实际接线在 internal/server。
package cert

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Renewer 执行单张证书的续期（由 cert.Service.Renew 实现）。
type Renewer interface {
	Renew(ctx context.Context, certID int) error
}

// DueLister 返回 horizon 内到期、可自动续期的证书 ID（由 cert.Service.ListDueForRenewal 实现）。
type DueLister interface {
	ListDue(ctx context.Context, horizon time.Duration) ([]int, error)
}

// Recorder 记录一次续期动作（成功/失败）作审计（由 server 层用 deploy.Service 实现为变更单）。
type Recorder interface {
	RecordRenewal(ctx context.Context, certID int, ok bool, detail string) error
}

// Scheduler 续期调度器。
type Scheduler struct {
	renewer    Renewer
	lister     DueLister
	recorder   Recorder
	horizon    time.Duration
	onCritical func(certID, fails int)
	failStreak map[int]int
	mu         sync.Mutex
	log        *slog.Logger
}

// NewScheduler 构造调度器。horizon<=0 时回落 30 天；onCritical 在连续失败达阈值时回调（可 nil）。
func NewScheduler(renewer Renewer, lister DueLister, recorder Recorder, horizon time.Duration, onCritical func(certID, fails int)) *Scheduler {
	if horizon <= 0 {
		horizon = 30 * 24 * time.Hour
	}
	return &Scheduler{
		renewer:    renewer,
		lister:     lister,
		recorder:   recorder,
		horizon:    horizon,
		onCritical: onCritical,
		failStreak: make(map[int]int),
		log:        slog.Default(),
	}
}

// SetLogger 覆盖日志出口。
func (s *Scheduler) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log = l
	}
}

// RenewDue 对所有到期证书触发续期，返回成功/失败 ID 列表。
// 连续失败达 3 次触发 onCritical；每次结果经 Recorder 审计。
func (s *Scheduler) RenewDue(ctx context.Context) (renewed, failed []int) {
	if s.lister == nil {
		return nil, nil
	}
	ids, err := s.lister.ListDue(ctx, s.horizon)
	if err != nil {
		s.logError("list due certs", err)
		return nil, nil
	}
	for _, id := range ids {
		rerr := s.renewer.Renew(ctx, id)
		ok := rerr == nil

		s.mu.Lock()
		if !ok {
			s.failStreak[id]++
			fails := s.failStreak[id]
			if fails >= 3 && s.onCritical != nil {
				s.onCritical(id, fails)
			}
		} else {
			s.failStreak[id] = 0
		}
		s.mu.Unlock()

		if !ok {
			failed = append(failed, id)
		} else {
			renewed = append(renewed, id)
		}

		if s.recorder != nil {
			detail := ""
			if rerr != nil {
				detail = rerr.Error()
			}
			if rerr2 := s.recorder.RecordRenewal(ctx, id, ok, detail); rerr2 != nil {
				s.logError("record renewal", rerr2)
			}
		}
	}
	return renewed, failed
}

// Start 在独立 goroutine 中按 interval 周期触发 RenewDue，首次对齐到当天 03:00（本地时区），
// 满足 T045 "每日 03:00 触发" 的契约；ctx 取消即退出（与进程同生命周期）。
func (s *Scheduler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	go func() {
		initial := nextRunAt(time.Now(), 3)
		select {
		case <-time.After(initial):
		case <-ctx.Done():
			return
		}
		s.RenewDue(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.RenewDue(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// nextRunAt 返回从 now 起到当天 hour:00（本地）的首次延迟；若已过则顺延次日。
func nextRunAt(now time.Time, hour int) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(now)
}

func (s *Scheduler) logError(scope string, err error) {
	if s.log != nil {
		s.log.Error("renew scheduler", "scope", scope, "error", err)
	}
}
