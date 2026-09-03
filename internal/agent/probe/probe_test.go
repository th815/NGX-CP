// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProber 可控探活器，用于复合测试。
type fakeProber struct{ ok bool }

func (f fakeProber) Probe(ctx context.Context) (*ProbeResult, error) {
	if f.ok {
		return &ProbeResult{OK: true, Detail: "ok"}, nil
	}
	return &ProbeResult{OK: false, Detail: "fail"}, nil
}

func TestHTTPProbe_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := NewHTTPProbe(srv.URL, 0, 2*time.Second)
	res, err := p.Probe(context.Background())
	require.NoError(t, err)
	assert.True(t, res.OK)
	assert.Contains(t, res.Detail, "200")
}

func TestHTTPProbe_502_Fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()
	p := NewHTTPProbe(srv.URL, 0, 2*time.Second)
	res, err := p.Probe(context.Background())
	require.NoError(t, err)
	assert.False(t, res.OK)
}

func TestHTTPProbe_ExpectCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	resOK, _ := NewHTTPProbe(srv.URL, 200, 2*time.Second).Probe(context.Background())
	assert.True(t, resOK.OK)
	res204, _ := NewHTTPProbe(srv.URL, 204, 2*time.Second).Probe(context.Background())
	assert.False(t, res204.OK, "期望 204 但返回 200 应失败")
}

func TestHTTPProbe_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	p := NewHTTPProbe(srv.URL, 0, 300*time.Millisecond)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := p.Probe(ctx)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.False(t, res.OK, "超时连接应判定不健康")
	assert.Less(t, elapsed, 2*time.Second, "超时必须在 Timeout 内返回，不能卡死")
}

func TestTCPProbe_ConnectOK(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			c.Close()
		}
	}()
	p := &TCPProbe{Addr: ln.Addr().String(), Timeout: 2 * time.Second}
	res, err := p.Probe(context.Background())
	require.NoError(t, err)
	assert.True(t, res.OK)
}

func TestTCPProbe_Refused(t *testing.T) {
	p := &TCPProbe{Addr: "127.0.0.1:1", Timeout: 2 * time.Second}
	res, err := p.Probe(context.Background())
	require.NoError(t, err)
	assert.False(t, res.OK)
}

func TestLogErrorProbe_NewErrors_Fails(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "error.log")
	require.NoError(t, os.WriteFile(logPath, []byte("[error] reload 前的历史错误\n"), 0o644))

	p, err := NewLogErrorProbe(ProbeConfig{LogPath: logPath, Window: 50 * time.Millisecond, MaxNewErrors: 1})
	require.NoError(t, err)

	// 在探测等待窗口内追加新错误（模拟 reload 后异常）
	go func() {
		time.Sleep(10 * time.Millisecond)
		f, ferr := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if ferr == nil {
			_, _ = f.WriteString("[emerg] 新致命错误\n[error] 又一条错误\n")
			f.Close()
		}
	}()

	res, err := p.Probe(context.Background())
	require.NoError(t, err)
	assert.False(t, res.OK, "观测窗口内新增错误超上限应失败")
}

func TestLogErrorProbe_NoNew_OK(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "error.log")
	require.NoError(t, os.WriteFile(logPath, []byte("normal access log\n"), 0o644))

	p, err := NewLogErrorProbe(ProbeConfig{LogPath: logPath, Window: 30 * time.Millisecond, MaxNewErrors: 1})
	require.NoError(t, err)
	res, err := p.Probe(context.Background())
	require.NoError(t, err)
	assert.True(t, res.OK, "无新增错误应健康")
}

func TestComposite_AllPass(t *testing.T) {
	cp := NewComposite(&fakeProber{ok: true}, &fakeProber{ok: true})
	res, err := cp.Probe(context.Background())
	require.NoError(t, err)
	assert.True(t, res.OK)
}

func TestComposite_OneFails(t *testing.T) {
	cp := NewComposite(&fakeProber{ok: true}, &fakeProber{ok: false})
	res, err := cp.Probe(context.Background())
	require.NoError(t, err)
	assert.False(t, res.OK)
	assert.Contains(t, res.Detail, "失败的探活")
}

func TestNew_UnknownType(t *testing.T) {
	_, err := New(ProbeConfig{Type: "nope"})
	assert.Error(t, err)
}

func TestNew_HTTP(t *testing.T) {
	p, err := New(ProbeConfig{Type: ProbeHTTP, URL: "http://x"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}
