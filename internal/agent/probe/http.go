// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package probe

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// HTTPProbe 通过 GET 一个 URL 判断健康。
type HTTPProbe struct {
	URL        string
	ExpectCode int // 0 表示接受 <500
	Timeout    time.Duration
	Client     *http.Client
}

// NewHTTPProbe 构造 HTTP 探活（ExpectCode=0 表示接受任意 <500）。
func NewHTTPProbe(url string, expectCode int, timeout time.Duration) *HTTPProbe {
	return &HTTPProbe{URL: url, ExpectCode: expectCode, Timeout: timeout}
}

// Probe 执行一次 HTTP 探活，受 ctx 与 Timeout 双重约束。
func (p *HTTPProbe) Probe(ctx context.Context) (*ProbeResult, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return &ProbeResult{OK: false, Detail: err.Error(), CheckedAt: time.Now()}, err
	}
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return &ProbeResult{OK: false, Detail: err.Error(), Latency: latency, CheckedAt: time.Now()}, nil
	}
	defer resp.Body.Close()
	ok := httpOK(resp.StatusCode, p.ExpectCode)
	detail := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if !ok {
		detail = fmt.Sprintf("HTTP %d 不符合健康期望", resp.StatusCode)
	}
	return &ProbeResult{OK: ok, Detail: detail, Latency: latency, CheckedAt: time.Now()}, nil
}

// httpOK 判定状态码是否健康。
func httpOK(code, expect int) bool {
	if expect == 0 {
		return code < 500
	}
	return code == expect
}
