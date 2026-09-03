// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package probe 实现发布后的节点健康检查（T033）。
//
// 提供四类探活：本地 HTTP（http）、端口连通（tcp）、错误日志增量（log_error）、
// 外部地址探活（external，复用 http 逻辑访问 VIP/外部端点）。CompositeProbe 将多个
// 探活器组合为「全部通过才健康」的复合探活。所有探活器统一实现 Prober 接口，
// 通过 New(ProbeConfig) 按配置构造。DeployExecutor 经 executor.probeAdapter 接入本包。
package probe

import (
	"context"
	"fmt"
	"time"
)

// ProbeType 是探活类型。
type ProbeType string

const (
	ProbeHTTP     ProbeType = "http"      // 本地 HTTP 探活
	ProbeTCP      ProbeType = "tcp"       // 端口连通
	ProbeLogError ProbeType = "log_error" // ★ 错误日志增量
	ProbeExternal ProbeType = "external"  // ★ 外部探活（复用 http，访问 VIP/外部端点）
)

// ProbeConfig 描述一次探活的参数。
type ProbeConfig struct {
	Type         ProbeType     // 探活类型
	URL          string        // http/external：目标 URL，如 http://127.0.0.1/healthz
	Addr         string        // tcp：地址，如 "127.0.0.1:80"
	ExpectCode   int           // http/external：期望状态码；0 表示接受 <500
	Timeout      time.Duration // 单次超时，默认 5s
	Retries      int           // 重试次数（含首次），默认 1
	LogPath      string        // log_error：日志文件路径
	ErrorPattern string        // log_error：错误匹配正则（空则用 nginx 级别关键字）
	MaxNewErrors int           // log_error：观测窗口内允许的新增错误数，默认 3
	Window       time.Duration // log_error：观测窗口，默认 30s
}

// ProbeResult 是一次探活的结果。
type ProbeResult struct {
	OK        bool
	Detail    string
	Latency   time.Duration
	CheckedAt time.Time
}

// Prober 是探活器接口。
type Prober interface {
	Probe(ctx context.Context) (*ProbeResult, error)
}

// New 按配置构造对应类型的探活器。
// external 复用 http 逻辑（访问外部 VIP/端点）；tcp 用 Addr；log_error 用 LogPath。
func New(cfg ProbeConfig) (Prober, error) {
	switch cfg.Type {
	case ProbeHTTP, ProbeExternal:
		return NewHTTPProbe(cfg.URL, cfg.ExpectCode, cfg.Timeout), nil
	case ProbeTCP:
		return &TCPProbe{Addr: cfg.Addr, Timeout: cfg.Timeout}, nil
	case ProbeLogError:
		return NewLogErrorProbe(cfg)
	default:
		return nil, fmt.Errorf("未知探活类型: %q", cfg.Type)
	}
}
