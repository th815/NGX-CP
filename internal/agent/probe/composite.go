// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package probe

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CompositeProbe 将多个探活器组合为「全部通过才健康」的复合探活（AND 语义）。
// 对应 T033 的双层探活：本地 HTTP + 日志增量 + 远程 VIP，任一失败即判定不健康。
type CompositeProbe struct {
	probers []Prober
	labels  []string
}

// NewComposite 直接组合已构造的探活器。
func NewComposite(probers ...Prober) *CompositeProbe {
	return &CompositeProbe{probers: probers}
}

// Composite 按配置列表构造复合探活器。
func Composite(cfgs []ProbeConfig) (*CompositeProbe, error) {
	cp := &CompositeProbe{}
	for i, cfg := range cfgs {
		p, err := New(cfg)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个探活配置无效: %w", i+1, err)
		}
		cp.probers = append(cp.probers, p)
		cp.labels = append(cp.labels, string(cfg.Type))
	}
	return cp, nil
}

// Probe 依次执行所有探活器，全部 OK 才 OK。
func (c *CompositeProbe) Probe(ctx context.Context) (*ProbeResult, error) {
	var fails, details []string
	for i, p := range c.probers {
		res, err := p.Probe(ctx)
		if err != nil {
			return nil, err
		}
		if !res.OK {
			label := "probe"
			if i < len(c.labels) {
				label = c.labels[i]
			}
			fails = append(fails, label)
			details = append(details, res.Detail)
		} else if res.Detail != "" {
			details = append(details, res.Detail)
		}
	}
	ok := len(fails) == 0
	detail := strings.Join(details, "; ")
	if !ok {
		detail = fmt.Sprintf("失败的探活: %s; %s", strings.Join(fails, ","), detail)
	}
	return &ProbeResult{OK: ok, Detail: detail, CheckedAt: time.Now()}, nil
}
