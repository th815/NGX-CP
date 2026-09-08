package logtail

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// queueEntry 是磁盘队列中的一个条目：一批日志 + 入队时间。
type queueEntry struct {
	TS    int64     `json:"ts"` // unix 纳秒，用于 24h 保留裁剪
	Lines []LogLine `json:"lines"`
}

// diskQueue 是断连时的本地 store-forward 队列：以 JSONL 追加写入，
// 文件本身即持久化介质，进程崩溃后重启可回放。保留窗口由 retention
// 控制（默认 24h），过期的条目在 Drain 时丢弃。
type diskQueue struct {
	path      string
	retention time.Duration
}

func newDiskQueue(path string, retention time.Duration) *diskQueue {
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	return &diskQueue{path: path, retention: retention}
}

// Add 追加一批日志到队列尾部。写入失败返回 error（调用方应保留内存批次重试）。
func (q *diskQueue) Add(batch []LogLine) error {
	if len(batch) == 0 {
		return nil
	}
	if dir := filepath.Dir(q.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(q.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	entry := queueEntry{TS: time.Now().UnixNano(), Lines: batch}
	return enc.Encode(entry)
}

// Pending 报告队列是否非空（文件存在且大于 0 字节）。
func (q *diskQueue) Pending() bool {
	fi, err := os.Stat(q.path)
	if err != nil {
		return false
	}
	return fi.Size() > 0
}

// Drain 回放队列中所有未过期条目：合并为单批交给 emit，emit 成功则清空
// 队列；emit 失败则保留队列并返回该 error（下次循环重试）。过期条目在
// 回放时直接丢弃（不计入 emit）。
func (q *diskQueue) Drain(emit func([]LogLine) error) error {
	f, err := os.Open(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	var combined []LogLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	cutoff := time.Now().Add(-q.retention).UnixNano()
	keptStale := false
	for sc.Scan() {
		var e queueEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// 损坏行：跳过（不可因单行坏数据卡死回放）。
			keptStale = true
			continue
		}
		if e.TS < cutoff {
			// 过期：丢弃。
			keptStale = true
			continue
		}
		combined = append(combined, e.Lines...)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(combined) == 0 {
		// 无有效待发，但可能有过期/损坏行需清理。
		if keptStale {
			return q.truncate()
		}
		return nil
	}
	if err := emit(combined); err != nil {
		return err
	}
	return q.truncate()
}

// truncate 清空队列文件（emit 成功后调用）。
func (q *diskQueue) truncate() error {
	// 用 0 长度截断而非删除，避免下次 Add 重新建文件的开销与竞态。
	return os.Truncate(q.path, 0)
}
