// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// 本文件落地 T026 的定时巡检 worker：周期性对缓存的节点实际配置重新运行 Detect，
// 刷新内存态漂移报告。worker 不主动拉取节点配置（避免每次跑 nginx -T 的开销），
// 实际配置来自 Agent 上报（RecordActual），worker 仅做基于 SHA 的廉价复检。
package config

import (
	"context"
	"log/slog"
	"time"
)

// RunWorker 启动定时巡检：每 interval 对所有已有缓存 actual 的节点复检一次。
// interval <= 0 时回落到 DriftConfig.CheckInterval。ctx 取消即退出。
func (d *DriftDetector) RunWorker(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = d.cfg.CheckInterval
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	slog.Default().Info("drift worker started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			slog.Default().Info("drift worker stopped")
			return ctx.Err()
		case <-ticker.C:
			d.scanAll(ctx)
		}
	}
}

// scanAll 对缓存的节点实际配置快照重新运行 Detect，刷新报告。
// 仅遍历已有 actual 快照的节点；尚未上报过的节点跳过（待首次上报填充）。
func (d *DriftDetector) scanAll(ctx context.Context) {
	d.mu.RLock()
	snapshot := make([]struct {
		nodeID int
		actual []ReportedConfigFile
	}, 0, len(d.actuals))
	for id, a := range d.actuals {
		snapshot = append(snapshot, struct {
			nodeID int
			actual []ReportedConfigFile
		}{id, a})
	}
	d.mu.RUnlock()

	for _, s := range snapshot {
		report, err := d.Detect(ctx, s.nodeID, s.actual)
		if err != nil {
			slog.Default().Warn("drift scan skipped", "node_id", s.nodeID, "err", err)
			continue
		}
		d.mu.Lock()
		d.reports[s.nodeID] = report
		d.mu.Unlock()
	}
}
