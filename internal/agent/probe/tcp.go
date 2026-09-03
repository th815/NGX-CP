// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package probe

import (
	"context"
	"fmt"
	"net"
	"time"
)

// TCPProbe 通过 Dial 一个 TCP 地址判断端口是否连通。
type TCPProbe struct {
	Addr    string
	Timeout time.Duration
}

// Probe 尝试建立一次 TCP 连接。
func (p *TCPProbe) Probe(ctx context.Context) (*ProbeResult, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	start := time.Now()
	d := net.Dialer{Timeout: timeout}
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := d.DialContext(ctx2, "tcp", p.Addr)
	latency := time.Since(start)
	if err != nil {
		return &ProbeResult{OK: false, Detail: err.Error(), Latency: latency, CheckedAt: time.Now()}, nil
	}
	_ = conn.Close()
	return &ProbeResult{OK: true, Detail: fmt.Sprintf("TCP %s 连通", p.Addr), Latency: latency, CheckedAt: time.Now()}, nil
}
