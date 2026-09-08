package logstore

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Ingester 攒批缓冲：把 Entry 缓冲到 batchSize 或 flushEvery 触发后
// 批量交给 Storage。满足 T062 "攒批异步写入" 的硬约束（单条插 ClickHouse 会拖垮）。
//
// 设计边界：Ingester 不感知来源（Agent gRPC 上报 / 测试直接喂），只负责
// 缓冲 + flush。来源转换（LogLine→Entry）由 FromLogLines 完成。
type Ingester struct {
	store      Storage
	batchSize  int
	flushEvery time.Duration
	log        *slog.Logger

	mu   sync.Mutex
	buf  []Entry
	stop chan struct{}
	wg   sync.WaitGroup
}

// IngesterOption 配置项。
type IngesterOption func(*Ingester)

// WithBatchSize 设置达量 flush 阈值（默认 1000）。
func WithBatchSize(n int) IngesterOption {
	return func(i *Ingester) {
		if n > 0 {
			i.batchSize = n
		}
	}
}

// WithFlushEvery 设置周期 flush 间隔（默认 5s）。
func WithFlushEvery(d time.Duration) IngesterOption {
	return func(i *Ingester) {
		if d > 0 {
			i.flushEvery = d
		}
	}
}

// WithLogger 设置日志（默认 slog.Default）。
func WithLogger(l *slog.Logger) IngesterOption {
	return func(i *Ingester) {
		if l != nil {
			i.log = l
		}
	}
}

// NewIngester 构造攒批器。
func NewIngester(store Storage, opts ...IngesterOption) *Ingester {
	i := &Ingester{
		store:      store,
		batchSize:  1000,
		flushEvery: 5 * time.Second,
		log:        slog.Default(),
		stop:       make(chan struct{}),
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

// Start 启动周期 flush goroutine。ctx 取消或 Close 都会触发最终 flush。
func (i *Ingester) Start(ctx context.Context) {
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		t := time.NewTicker(i.flushEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = i.Flush(context.Background())
				return
			case <-i.stop:
				_ = i.Flush(context.Background())
				return
			case <-t.C:
				if err := i.Flush(context.Background()); err != nil {
					i.log.Error("logstore flush", "error", err)
				}
			}
		}
	}()
}

// Accept 接收一批 Entry（通常来自 Agent gRPC 上报后的 FromLogLines 转换）。
// 缓冲达到 batchSize 立即 flush。
func (i *Ingester) Accept(entries []Entry) {
	if len(entries) == 0 {
		return
	}
	i.mu.Lock()
	i.buf = append(i.buf, entries...)
	n := len(i.buf)
	i.mu.Unlock()
	if n >= i.batchSize {
		_ = i.Flush(context.Background())
	}
}

// Flush 把当前缓冲交给 Storage（空缓冲直接返回）。
// 入库失败时把这批重新放回缓冲头部，下次再试，避免丢行。
func (i *Ingester) Flush(ctx context.Context) error {
	i.mu.Lock()
	if len(i.buf) == 0 {
		i.mu.Unlock()
		return nil
	}
	batch := i.buf
	i.buf = nil
	i.mu.Unlock()

	if err := i.store.Ingest(ctx, batch); err != nil {
		i.mu.Lock()
		i.buf = append(batch, i.buf...)
		i.mu.Unlock()
		return err
	}
	return nil
}

// Close 停止周期 flush 并刷出剩余缓冲。幂等。
func (i *Ingester) Close() error {
	select {
	case <-i.stop:
	default:
		close(i.stop)
	}
	i.wg.Wait()
	return i.Flush(context.Background())
}
