package logtail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// jsonLine 生成一条符合 T060 契约的 JSON 访问日志行。
func jsonLine(status int, rid string) string {
	return fmt.Sprintf(
		`{"time":"2026-09-08T00:00:00+00:00","rid":%q,"remote_addr":"10.0.0.1",`+
			`"server":"example","uri":"/x","status":%d,"upstream_addr":"10.0.0.2:80",`+
			`"upstream_status":"%d","upstream_rt":0.01,"request_rt":0.02,"bytes":123,"ua":"curl"}`,
		rid, status, status)
}

func TestParseLine(t *testing.T) {
	l := ParseLine(jsonLine(200, "abc"), "node-1")
	if l.Status != 200 {
		t.Fatalf("status want 200 got %d", l.Status)
	}
	if l.RID != "abc" {
		t.Fatalf("rid want abc got %q", l.RID)
	}
	if l.Node != "node-1" {
		t.Fatalf("node want node-1 got %q", l.Node)
	}
	if l.IsErrorClass() {
		t.Fatalf("200 should not be error class")
	}

	// 损坏行：Raw 保留，Status 0。
	bad := ParseLine("this is not json", "n")
	if bad.Status != 0 || bad.Raw != "this is not json" {
		t.Fatalf("bad line should keep raw, got %+v", bad)
	}
	if bad.IsErrorClass() {
		t.Fatalf("status 0 must NOT be error class")
	}
	// 但采样下应保留（无法解析不丢）。
	if !sampleKeep(bad, 0.01, func() float64 { return 1.0 }) {
		t.Fatalf("unparseable (status 0) must be kept under sampling")
	}
	if !ParseLine(jsonLine(500, "x"), "n").IsErrorClass() {
		t.Fatalf("500 should be error class")
	}
}

func TestSampleKeep(t *testing.T) {
	rngOne := func() float64 { return 1.0 }   // 永远"不抽中"
	rngZero := func() float64 { return 0.0 }  // 永远"抽中"

	ok := LogLine{Status: 200}
	// rate=0.5，rng=1 → 1<0.5 false → 丢弃
	if sampleKeep(ok, 0.5, rngOne) {
		t.Fatalf("2xx should be dropped at rate<1 with rng=1")
	}
	// rate=0.5，rng=0 → 0<0.5 true → 保留
	if !sampleKeep(ok, 0.5, rngZero) {
		t.Fatalf("2xx should be kept at rate<1 with rng=0")
	}
	// error 类恒保留，无视 rate/rng
	errLine := LogLine{Status: 404}
	if !sampleKeep(errLine, 0.01, rngOne) {
		t.Fatalf("4xx must never be dropped")
	}
	// rate>=1 全采
	if !sampleKeep(ok, 1, rngOne) {
		t.Fatalf("rate>=1 should keep all")
	}
}

func TestOffsetRoundtripAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "off")
	if err := saveOffset(p, 12345); err != nil {
		t.Fatal(err)
	}
	v, err := loadOffset(p)
	if err != nil || v != 12345 {
		t.Fatalf("roundtrip want 12345 got %d err=%v", v, err)
	}
	// 损坏文件 → 0
	if err := os.WriteFile(p, []byte("not-a-number"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err = loadOffset(p)
	if err != nil || v != 0 {
		t.Fatalf("corrupt offset should fall back to 0, got %d err=%v", v, err)
	}
	// 不存在 → 0
	v, err = loadOffset(filepath.Join(dir, "missing"))
	if err != nil || v != 0 {
		t.Fatalf("missing offset should be 0, got %d err=%v", v, err)
	}
}

// blockingEmitter 线程安全地收集 emit 的日志行，并支持失败计数。
type blockingEmitter struct {
	mu      sync.Mutex
	lines   []LogLine
	failFor int32 // 前 N 次 emit 返回 error
	calls   int32
}

func (b *blockingEmitter) emit(lines []LogLine) error {
	n := atomic.AddInt32(&b.calls, 1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= atomic.LoadInt32(&b.failFor) {
		return fmt.Errorf("simulated emit failure #%d", n)
	}
	b.lines = append(b.lines, lines...)
	return nil
}

func (b *blockingEmitter) snapshot() []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LogLine, len(b.lines))
	copy(out, b.lines)
	return out
}

func TestTailerBasicAndRestart(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")

	// 预写 3 行。
	content := jsonLine(200, "r1") + "\n" + jsonLine(500, "r2") + "\n" + jsonLine(200, "r3") + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	em := &blockingEmitter{}
	tl := &Tailer{
		Path:         logPath,
		OffsetFile:   filepath.Join(dir, "access.offset"),
		QueueDir:     filepath.Join(dir, "q"),
		Node:         "node-1",
		SampleRate:   1, // 全采
		BatchSize:    2,
		FlushEvery:   20 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Rand:         func() float64 { return 0 },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx, em.emit) }()

	// 等待首轮读取完成。
	deadline := time.Now().Add(300 * time.Millisecond)
	for len(em.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := em.snapshot()
	if len(got) != 3 {
		t.Fatalf("want 3 lines emitted, got %d: %+v", len(got), got)
	}
	// 500 必须存在（error 类）。
	found500 := false
	for _, l := range got {
		if l.Status == 500 {
			found500 = true
		}
	}
	if !found500 {
		t.Fatalf("500 line missing: %+v", got)
	}

	// offset 文件应已写。
	if _, err := os.Stat(tl.OffsetFile); err != nil {
		t.Fatalf("offset file not written: %v", err)
	}

	cancel()
	<-done

	// 追加 2 行后，用同 OffsetFile 的新 Tailer 应从断点续读，只发新行。
	more := jsonLine(200, "r4") + "\n" + jsonLine(403, "r5") + "\n"
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(more)
	f.Close()

	em2 := &blockingEmitter{}
	tl2 := &Tailer{
		Path:         logPath,
		OffsetFile:   tl.OffsetFile, // 复用同一 offset 文件
		QueueDir:     filepath.Join(dir, "q2"),
		Node:         "node-1",
		SampleRate:   1,
		BatchSize:    2,
		FlushEvery:   20 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Rand:         func() float64 { return 0 },
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	done2 := make(chan error, 1)
	go func() { done2 <- tl2.Run(ctx2, em2.emit) }()

	deadline2 := time.Now().Add(300 * time.Millisecond)
	for len(em2.snapshot()) < 2 && time.Now().Before(deadline2) {
		time.Sleep(5 * time.Millisecond)
	}
	got2 := em2.snapshot()
	if len(got2) != 2 {
		t.Fatalf("restart want 2 new lines, got %d: %+v", len(got2), got2)
	}
	for _, l := range got2 {
		if l.RID != "r4" && l.RID != "r5" {
			t.Fatalf("restart should only emit new lines, got rid=%s", l.RID)
		}
	}
	cancel2()
	<-done2
}

func TestTailerLogrotate(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")

	// 初始文件 2 行。
	content := jsonLine(200, "a1") + "\n" + jsonLine(200, "a2") + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	em := &blockingEmitter{}
	tl := &Tailer{
		Path:         logPath,
		OffsetFile:   filepath.Join(dir, "off"),
		QueueDir:     filepath.Join(dir, "q"),
		Node:         "n",
		SampleRate:   1,
		BatchSize:    100,
		FlushEvery:   10 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Rand:         func() float64 { return 0 },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx, em.emit) }()

	// 等首 2 行被读。
	deadline := time.Now().Add(300 * time.Millisecond)
	for len(em.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// 模拟 logrotate：把原文件改名，新建同名空文件再写入新行（新 inode）。
	rotated := filepath.Join(dir, "access.log.1")
	if err := os.Rename(logPath, rotated); err != nil {
		t.Fatal(err)
	}
	newContent := jsonLine(200, "b1") + "\n" + jsonLine(200, "b2") + "\n" + jsonLine(200, "b3") + "\n"
	if err := os.WriteFile(logPath, []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline2 := time.Now().Add(400 * time.Millisecond)
	for len(em.snapshot()) < 5 && time.Now().Before(deadline2) {
		time.Sleep(5 * time.Millisecond)
	}
	got := em.snapshot()
	if len(got) != 5 {
		t.Fatalf("after rotate want 5 lines, got %d: %+v", len(got), got)
	}
	// 确认包含 b 系列（来自新 inode 文件）。
	hasB := false
	for _, l := range got {
		if l.RID == "b1" || l.RID == "b2" || l.RID == "b3" {
			hasB = true
		}
	}
	if !hasB {
		t.Fatalf("rotated file lines not collected: %+v", got)
	}
	cancel()
	<-done
}

func TestTailerStoreForward(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	content := jsonLine(200, "s1") + "\n" + jsonLine(200, "s2") + "\n" + jsonLine(200, "s3") + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 前 2 次 emit 失败 → 进队列；之后成功 → 回放。
	em := &blockingEmitter{failFor: 2}
	tl := &Tailer{
		Path:         logPath,
		OffsetFile:   filepath.Join(dir, "off"),
		QueueDir:     filepath.Join(dir, "q"),
		Node:         "n",
		SampleRate:   1,
		BatchSize:    100,
		FlushEvery:   10 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Rand:         func() float64 { return 0 },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx, em.emit) }()

	deadline := time.Now().Add(500 * time.Millisecond)
	for len(em.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := em.snapshot()
	if len(got) != 3 {
		t.Fatalf("store-forward want all 3 lines eventually, got %d (calls=%d): %+v",
			len(got), atomic.LoadInt32(&em.calls), got)
	}
	if atomic.LoadInt32(&em.calls) < 3 {
		t.Fatalf("expected >=3 emit attempts (2 fail + >=1 success), got %d", atomic.LoadInt32(&em.calls))
	}
	cancel()
	<-done
}
