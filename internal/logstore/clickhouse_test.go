package logstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/internal/agent/logtail"
)

func sampleLines(n int) []logtail.LogLine {
	out := make([]logtail.LogLine, n)
	for i := range out {
		out[i] = logtail.LogLine{
			TS:         "2026-09-08T10:00:00+08:00",
			Node:       "n1",
			RID:        "r",
			RemoteAddr: "1.2.3.4",
			Server:     "example.com",
			URI:        "/a",
			Status:     200,
			Bytes:      100,
			UA:         "ua",
		}
	}
	return out
}

func TestFromLogLines(t *testing.T) {
	lines := []logtail.LogLine{
		{TS: "2026-09-08T10:00:00+08:00", Node: "n1", Status: 500, RemoteAddr: "1.1.1.1"},
		{TS: "bad", Node: "n2", Status: 200},
	}
	es := FromLogLines(lines, nil)
	require.Len(t, es, 2)
	assert.Equal(t, uint16(500), es[0].Status)
	assert.Equal(t, "n1", es[0].Node)
	// 坏 TS → 零值，但不丢行
	assert.True(t, es[1].TS.IsZero())
	assert.Equal(t, "n2", es[1].Node)
}

func TestIngester_BatchSizeFlush(t *testing.T) {
	mem := NewMemStorage()
	ing := NewIngester(mem, WithBatchSize(3))
	ing.Accept(FromLogLines(sampleLines(2), nil))
	assert.Equal(t, 0, mem.Len(), "未达 batchSize 不应 flush")
	ing.Accept(FromLogLines(sampleLines(1), nil))
	assert.Equal(t, 3, mem.Len(), "达到 batchSize 应 flush")
	_ = ing.Close()
}

func TestIngester_IntervalFlush(t *testing.T) {
	mem := NewMemStorage()
	ing := NewIngester(mem, WithBatchSize(1000), WithFlushEvery(20*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ing.Start(ctx)
	ing.Accept(FromLogLines(sampleLines(1), nil))
	assert.Equal(t, 0, mem.Len(), "周期未到不应 flush")
	require.Eventually(t, func() bool { return mem.Len() == 1 }, 500*time.Millisecond, 10*time.Millisecond)
	_ = ing.Close()
}

func TestIngester_CloseFlushesRemainder(t *testing.T) {
	mem := NewMemStorage()
	ing := NewIngester(mem, WithBatchSize(1000))
	ing.Accept(FromLogLines(sampleLines(2), nil))
	require.NoError(t, ing.Close())
	assert.Equal(t, 2, mem.Len(), "Close 应刷出剩余缓冲")
}

func TestIngester_FlushErrorRebuffers(t *testing.T) {
	mem := NewMemStorage()
	mem.SetFail(true)
	ing := NewIngester(mem, WithBatchSize(1000))
	ing.Accept(FromLogLines(sampleLines(1), nil))
	require.Error(t, ing.Flush(context.Background()), "入库失败应返回错误")
	// 恢复后可成功入库，证明失败批次被保留而非丢弃
	mem.SetFail(false)
	require.NoError(t, ing.Flush(context.Background()))
	assert.Equal(t, 1, mem.Len(), "失败后缓冲应保留，恢复后可成功入库")
	_ = ing.Close()
}

func TestIngester_CloseReturnsFlushError(t *testing.T) {
	mem := NewMemStorage()
	mem.SetFail(true)
	ing := NewIngester(mem, WithBatchSize(1000))
	ing.Accept(FromLogLines(sampleLines(1), nil))
	require.Error(t, ing.Close(), "Close 应透传 flush 错误")
}

func TestParseMemLimit(t *testing.T) {
	cases := map[string]int64{
		"":            0,
		"6G":          6 * 1024 * 1024 * 1024,
		"6GB":         6 * 1024 * 1024 * 1024,
		"512M":        512 * 1024 * 1024,
		"1024":        1024,
		"6442450944":  6442450944,
	}
	for in, want := range cases {
		got, err := ParseMemLimit(in)
		require.NoError(t, err, "input %q", in)
		assert.Equal(t, want, got, "input %q", in)
	}
	_, err := ParseMemLimit("7X")
	assert.Error(t, err, "未知单位应报错")
}
