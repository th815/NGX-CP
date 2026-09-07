// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package acme

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/internal/dns"
)

// fakeDNS 是 dns.DNSProvider 的内存实现，记录调用以便断言。
type fakeDNS struct {
	setCalls []setCall
	delCalls []delCall
}

type setCall struct{ zone, name, value string }
type delCall struct{ zone, name string }

func (f *fakeDNS) Name() string { return "fake" }
func (f *fakeDNS) SetRecord(_ context.Context, zone, name, value string) error {
	f.setCalls = append(f.setCalls, setCall{zone, name, value})
	return nil
}
func (f *fakeDNS) DeleteRecord(_ context.Context, zone, name string) error {
	f.delCalls = append(f.delCalls, delCall{zone, name})
	return nil
}
func (f *fakeDNS) Validate(_ context.Context) error { return nil }

func TestLegoSolver_Present(t *testing.T) {
	f := &fakeDNS{}
	s := &legoSolver{provider: f}
	const domain = "example.com"
	const keyAuth = "test-key-auth"
	require.NoError(t, s.Present(domain, "token", keyAuth))

	require.Len(t, f.setCalls, 1)
	info := dns01.GetChallengeInfo(domain, keyAuth)
	require.Equal(t, info.Value, f.setCalls[0].value)
	require.Equal(t, strings.TrimSuffix(info.EffectiveFQDN, "."), f.setCalls[0].name)
	require.Equal(t, domain, f.setCalls[0].zone)
}

func TestLegoSolver_PresentWildcard(t *testing.T) {
	f := &fakeDNS{}
	s := &legoSolver{provider: f}
	const domain = "*.example.com"
	require.NoError(t, s.Present(domain, "token", "k"))

	require.Len(t, f.setCalls, 1)
	// 通配符：zone 应剥离 *. 前缀取根域。
	require.Equal(t, "example.com", f.setCalls[0].zone)
	require.Contains(t, f.setCalls[0].name, "_acme-challenge")
}

func TestLegoSolver_CleanUp(t *testing.T) {
	f := &fakeDNS{}
	s := &legoSolver{provider: f}
	require.NoError(t, s.CleanUp("example.com", "token", "k"))
	require.Len(t, f.delCalls, 1)
	require.Equal(t, "example.com", f.delCalls[0].zone)
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(50 * time.Millisecond)
	start := time.Now()
	require.NoError(t, rl.Wait(context.Background(), "example.com"))
	// 第二次应在 ~50ms 后被放行。
	require.NoError(t, rl.Wait(context.Background(), "example.com"))
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 40*time.Millisecond)
}

// 编译期断言：fakeDNS 满足 dns.DNSProvider（T041 接口），同时使 dns 导入有意义。
var _ dns.DNSProvider = (*fakeDNS)(nil)
