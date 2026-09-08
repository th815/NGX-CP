// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 tianhao

package lvs

import (
	"context"
	"log/slog"
	"time"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/director"
)

// slogLevelCritical 高于 Error(8)，用于脑裂等严重故障的 CRITICAL 级日志。
const slogLevelCritical = slog.Level(12)

// SplitBrainAlert 是脑裂检测的结果视图。
type SplitBrainAlert struct {
	Detected  bool     `json:"detected"`           // 是否检测到脑裂
	VIP       string   `json:"vip,omitempty"`      // 发生冲突的虚拟 IP
	Holders   []int    `json:"holders,omitempty"`  // 同时持 VIP 的 Director ID 列表
	Detail    string   `json:"detail,omitempty"`   // 人类可读说明
	CheckedAt int64    `json:"checked_at"`         // 检测时刻（unix 秒）
}

// directorHolder 是脑裂判定的单条输入（解耦 ent 依赖，便于纯函数单测）。
type directorHolder struct {
	ID         int
	HoldingVIP bool
	VIP        string
}

// DetectSplitBrain 纯函数判定：若存在 ≥2 个 Director 同时持 VIP → 脑裂。
// tolerance 预留（当前按布尔判定；新鲜度由 Agent 合规上报周期与 watch 间隔共同保证：
// 正常主备切换时仅有 1 个 MASTER 持 VIP，仅"双 MASTER 抢 VIP"的异常态才会 ≥2 持）。
func DetectSplitBrain(holders []directorHolder, _ time.Duration) (bool, []int, string) {
	active := make([]int, 0, 2)
	for _, h := range holders {
		if h.HoldingVIP {
			active = append(active, h.ID)
		}
	}
	if len(active) >= 2 {
		return true, active, "检测到脑裂：多个 Director 同时持有 VIP（LVS-DR 应仅一个 MASTER 持 VIP）"
	}
	return false, active, ""
}

// CheckSplitBrain 从 DB 读取全部 Director 的实时持 VIP 态并判定脑裂。
// 未上报 holding_vip 的 Director 按 state==MASTER 推断（与拓扑视图一致）。
func (s *Service) CheckSplitBrain(ctx context.Context) (*SplitBrainAlert, error) {
	ds, err := s.client.Director.Query().
		WithNode().
		Order(ent.Asc(director.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	holders := make([]directorHolder, 0, len(ds))
	for _, d := range ds {
		hold := d.HoldingVip
		if !hold {
			hold = string(d.State) == "MASTER"
		}
		holders = append(holders, directorHolder{ID: d.ID, HoldingVIP: hold, VIP: d.Vip})
	}
	detected, active, detail := DetectSplitBrain(holders, 0)
	return &SplitBrainAlert{
		Detected:  detected,
		VIP:       pickVIP(holders, active),
		Holders:   active,
		Detail:    detail,
		CheckedAt: time.Now().Unix(),
	}, nil
}

func pickVIP(holders []directorHolder, active []int) string {
	for _, id := range active {
		for _, h := range holders {
			if h.ID == id && h.VIP != "" {
				return h.VIP
			}
		}
	}
	return ""
}

// StartSplitBrainWatch 周期检测脑裂；检测到时回调 alertFn（控制面据此打 CRITICAL 日志/告警）。
// 阻塞直到 ctx 取消，应在独立 goroutine 中运行。
func (s *Service) StartSplitBrainWatch(ctx context.Context, interval time.Duration, alertFn func(*SplitBrainAlert)) {
	if interval <= 0 {
		interval = time.Minute
	}
	// 启动即首检一次。
	s.checkOnce(ctx, alertFn)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkOnce(ctx, alertFn)
		}
	}
}

func (s *Service) checkOnce(ctx context.Context, alertFn func(*SplitBrainAlert)) {
	alert, err := s.CheckSplitBrain(ctx)
	if err != nil {
		slog.Default().Error("split-brain check failed", "err", err)
		return
	}
	if alert.Detected {
		slog.Default().Log(ctx, slogLevelCritical, "SPLIT-BRAIN DETECTED: multiple Directors hold VIP",
			"vip", alert.VIP, "holders", alert.Holders, "detail", alert.Detail)
		if alertFn != nil {
			alertFn(alert)
		}
	}
}
