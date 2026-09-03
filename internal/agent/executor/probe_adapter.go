// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package executor

import (
	"context"

	"github.com/th/ngxcp/internal/agent/probe"
)

// probeAdapter 把 probe.Prober（返回 *ProbeResult）适配为本执行器的 Prober 接口
// （返回 (bool, string, error)），使 T033 复合探活可直接接入 DeployExecutor。
type probeAdapter struct{ p probe.Prober }

func (a probeAdapter) Probe(ctx context.Context) (bool, string, error) {
	res, err := a.p.Probe(ctx)
	if err != nil {
		return false, res.Detail, err
	}
	return res.OK, res.Detail, nil
}

// SetProbeConfigs 用一组探活配置构造复合探活器（全部通过才健康），替换默认 HTTP 探活。
// 典型用法：本地 HTTP + 日志增量 + 远程 VIP（对应 T033 双层探活）。
func (e *DeployExecutor) SetProbeConfigs(cfgs ...probe.ProbeConfig) error {
	cp, err := probe.Composite(cfgs)
	if err != nil {
		return err
	}
	e.prober = probeAdapter{cp}
	return nil
}

// 便捷别名与常量：调用方无需额外 import probe 包即可声明探活配置。
type (
	ProbeConfig = probe.ProbeConfig
	ProbeResult = probe.ProbeResult
	ProbeType   = probe.ProbeType
)

const (
	ProbeHTTP     = probe.ProbeHTTP
	ProbeTCP      = probe.ProbeTCP
	ProbeLogError = probe.ProbeLogError
	ProbeExternal = probe.ProbeExternal
)
