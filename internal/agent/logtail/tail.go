package logtail

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Tailer 采集单个 Nginx 访问日志文件。调用 Run 后阻塞直到 ctx 取消。
//
// 关键不变量：
//   - pos（已消费字节数）始终相对"当前打开的文件"；rotate 后重置为 0。
//   - 每轮循环结束都原子保存 offset，因此崩溃重启后能续读；
//     rotate 检测后立即把 pos 置 0 并保存，故重启不会误用旧文件的偏移。
//   - emit 失败时整批进磁盘队列，队列为空前的每一轮都尝试 Drain 回放。
type Tailer struct {
	Path       string // 日志文件路径（如 /var/log/nginx/access.log）
	OffsetFile string // 偏移量持久化文件，默认 Path+".offset"
	QueueDir   string // store-forward 队列目录，默认 Path+".queue"

	Node string // 注入到每条 LogLine.Node 的节点标识

	// Format 指定日志格式，由 runtime 从 CollectLogTargets 透传：
	//   "json"  → JSON 访问日志（T060 下发的标准格式）
	//   "combined" / "main" / "" → 标准 Nginx 文本格式（默认，存量业务最常见）
	//   其他未知值 → ParseLine 自动探测（以 '{' 开头按 JSON，否则按文本）
	Format string

	SampleRate float64 // 采样率 [0,1]，1=全采；error 类恒保留
	BatchSize  int     // 攒批大小，默认 1000
	FlushEvery time.Duration // 最大攒批间隔，默认 5s
	PollInterval time.Duration // 无新数据时的轮询间隔，默认 200ms
	MaxLineBytes int   // 单行上限，超长丢弃，默认 256KB
	Retention  time.Duration // 队列保留窗口，默认 24h

	// Rand 采样随机源，默认 math/rand.Float64；测试可注入确定性源。
	Rand func() float64
}

// inodeSizeOfFile 返回已打开文件的 inode 与当前大小。
func inodeSizeOfFile(f *os.File) (uint64, int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fi.Size(), nil
	}
	return st.Ino, fi.Size(), nil
}

// inodeSizeOfPath 返回路径当前 inode 与大小（用于 rotate/truncate 探测）。
func inodeSizeOfPath(path string) (uint64, int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fi.Size(), nil
	}
	return st.Ino, fi.Size(), nil
}

// openAt 打开 path 并 seek 到 offset（offset>文件大小则 seek 到末尾）。
func (t *Tailer) openAt(offset int64) (*os.File, uint64, error) {
	f, err := os.Open(t.Path)
	if err != nil {
		return nil, 0, err
	}
	ino, sz, err := inodeSizeOfFile(f)
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > sz {
		offset = sz
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, ino, nil
}

// parseLine 按 Tailer.Format 选择解析器：
//   - "json"            → JSON 解析（T060 下发的标准格式）
//   - "combined"/"main" → 标准 Nginx 文本解析
//   - "" 或其他未知值   → 自动探测（以 '{' 开头按 JSON，否则按文本）
//
// 注意：combined 目标的 Format 在 capability 中为空串（nginx 默认），
// 故 "" 走自动探测即可正确解析存量业务文本日志，无需显式标记。
func (t *Tailer) parseLine(raw string) LogLine {
	switch strings.ToLower(strings.TrimSpace(t.Format)) {
	case "json":
		return parseJSONLine(raw, t.Node)
	case "combined", "main":
		return parseStandardLine(raw, t.Node)
	default:
		return ParseLine(raw, t.Node) // 自动探测兜底（含 Format==""）
	}
}

// Run 启动采集循环，直到 ctx 取消。emit 用于把攒好的批次送出（下游可能
// 是上报控制面）。emit 返回 error 时批次进 store-forward 队列。
func (t *Tailer) Run(ctx context.Context, emit func([]LogLine) error) error {
	if t.Path == "" {
		return errors.New("logtail: empty Path")
	}
	if t.OffsetFile == "" {
		t.OffsetFile = t.Path + ".offset"
	}
	if t.QueueDir == "" {
		t.QueueDir = t.Path + ".queue"
	}
	if t.BatchSize <= 0 {
		t.BatchSize = 1000
	}
	if t.FlushEvery <= 0 {
		t.FlushEvery = 5 * time.Second
	}
	if t.PollInterval <= 0 {
		t.PollInterval = 200 * time.Millisecond
	}
	if t.SampleRate <= 0 {
		t.SampleRate = 1
	}
	if t.MaxLineBytes <= 0 {
		t.MaxLineBytes = 256 * 1024
	}
	if t.Retention <= 0 {
		t.Retention = 24 * time.Hour
	}
	if t.Rand == nil {
		t.Rand = rand.Float64
	}

	offset, err := loadOffset(t.OffsetFile)
	if err != nil {
		return err
	}

	var f *os.File
	var openInode uint64
	pos := offset
	if nf, ino, oerr := t.openAt(offset); oerr == nil {
		f = nf
		openInode = ino
	}
	// 文件可能尚不存在（Agent 先于 Nginx 启动）——f==nil，等循环中重开。

	queue := newDiskQueue(filepath.Join(t.QueueDir, "backlog.jsonl"), t.Retention)

	var reader *bufio.Reader
	if f != nil {
		reader = bufio.NewReaderSize(f, 64*1024)
	}

	batch := make([]LogLine, 0, t.BatchSize)

	// flush 把当前 batch 送出：成功则清空；失败则进队列（不丢）。
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		b := batch
		batch = make([]LogLine, 0, t.BatchSize)
		if err := emit(b); err != nil {
			if aerr := queue.Add(b); aerr != nil {
				// 队列也写不进：保留内存批次，下一轮重试。
				batch = append(batch, b...)
				return err
			}
			return nil
		}
		return nil
	}

	flushTicker := time.NewTicker(t.FlushEvery)
	defer flushTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = flush()
			if f != nil {
				_ = saveOffset(t.OffsetFile, pos)
			}
			return ctx.Err()
		case <-flushTicker.C:
			_ = flush()
		default:
		}

		// 1) 优先回放 store-forward 队列（断连补传）。
		if queue.Pending() {
			_ = queue.Drain(emit)
		}

		// 2) 从文件读取可用数据。
		if f != nil {
			eof, rerr := t.readAvailable(f, reader, &pos, &batch, flush)
			if rerr != nil {
				// 读错误（如文件被删）：关闭，下一轮重开。
				f.Close()
				f = nil
				reader = nil
			}
			if !eof && rerr == nil {
				// 还有数据可读（未到 EOF），继续循环读取，不 sleep。
				continue
			}
		}

		// 3) 文件未打开则尝试（重新）打开（等待 Nginx 创建或 rotate 后新文件）。
		if f == nil {
			if nf, ino, oerr := t.openAt(pos); oerr == nil {
				f = nf
				openInode = ino
				reader = bufio.NewReaderSize(f, 64*1024)
			}
		} else {
			// 4) rotate / truncate 探测。
			curInode, curSize, serr := inodeSizeOfPath(t.Path)
			if serr == nil {
				if curInode != openInode {
					// 文件被 rotate：旧 fd 已读到 EOF，重开新文件从头读。
					f.Close()
					if nf, ino, oerr := t.openAt(0); oerr == nil {
						f = nf
						openInode = ino
						reader = bufio.NewReaderSize(f, 64*1024)
						pos = 0
					} else {
						f = nil
						reader = nil
					}
				} else if curSize < pos {
					// 同 inode 被截断：重置到文件头。
					if _, serr := f.Seek(0, io.SeekStart); serr == nil {
						reader.Reset(f)
						pos = 0
					}
				}
			}
		}

		// 5) 周期保存 offset（保证崩溃续读）。
		_ = saveOffset(t.OffsetFile, pos)

		select {
		case <-ctx.Done():
		case <-time.After(t.PollInterval):
		}
	}
}

// readAvailable 从当前文件读取直到 EOF 或满批。返回 (eof, err)：
// eof=true 表示已读到文件末尾（应做 rotate 探测）；err!=nil 表示真实读错误。
// 每消费一行即推进 pos，并按 SampleRate 过滤（error 类恒保留）。
func (t *Tailer) readAvailable(f *os.File, r *bufio.Reader, pos *int64, batch *[]LogLine, flush func() error) (bool, error) {
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			trimmed := trimNewline(line)
			if trimmed != "" {
				l := t.parseLine(trimmed)
				if sampleKeep(l, t.SampleRate, t.Rand) {
					*batch = append(*batch, l)
				}
			}
			*pos += int64(len(line))
		}
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if len(*batch) >= t.BatchSize {
			if ferr := flush(); ferr != nil {
				return false, nil
			}
		}
	}
}
