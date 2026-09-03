// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package probe

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// LogErrorProbe 在观测窗口内检查错误日志的新增量。
// 必须在 reload 之后才开始记录 offset，否则 reload 前历史错误会被误算（见 T033 陷阱）。
type LogErrorProbe struct {
	LogPath      string
	ErrorPattern *regexp.Regexp
	MaxNewErrors int
	Window       time.Duration
}

// NewLogErrorProbe 按配置构造日志探活器。
func NewLogErrorProbe(cfg ProbeConfig) (*LogErrorProbe, error) {
	if cfg.LogPath == "" {
		return nil, fmt.Errorf("log_error 探活需要 LogPath")
	}
	window := cfg.Window
	if window <= 0 {
		window = 30 * time.Second
	}
	maxErr := cfg.MaxNewErrors
	if maxErr <= 0 {
		maxErr = 3
	}
	var re *regexp.Regexp
	if cfg.ErrorPattern != "" {
		var err error
		re, err = regexp.Compile(cfg.ErrorPattern)
		if err != nil {
			return nil, fmt.Errorf("ErrorPattern 编译失败: %w", err)
		}
	}
	return &LogErrorProbe{LogPath: cfg.LogPath, ErrorPattern: re, MaxNewErrors: maxErr, Window: window}, nil
}

// Probe 记录当前文件末尾偏移，等待窗口，再统计新增错误行数。
func (p *LogErrorProbe) Probe(ctx context.Context) (*ProbeResult, error) {
	off, err := tailOffset(p.LogPath)
	if err != nil {
		return &ProbeResult{OK: false, Detail: err.Error(), CheckedAt: time.Now()}, nil
	}
	select {
	case <-ctx.Done():
		return &ProbeResult{OK: false, Detail: "探活被取消: " + ctx.Err().Error(), CheckedAt: time.Now()}, nil
	case <-time.After(p.Window):
	}
	count, sample, err := countErrorsSince(p.LogPath, off, p.ErrorPattern)
	if err != nil {
		return &ProbeResult{OK: false, Detail: err.Error(), CheckedAt: time.Now()}, nil
	}
	ok := count <= p.MaxNewErrors
	detail := fmt.Sprintf("观测窗口 %s 内新增错误 %d 条（上限 %d）", p.Window, count, p.MaxNewErrors)
	if !ok {
		detail = fmt.Sprintf("%s；样本: %s", detail, sample)
	}
	return &ProbeResult{OK: ok, Detail: detail, CheckedAt: time.Now()}, nil
}

// tailOffset 取文件当前大小作为观测起点。
func tailOffset(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// countErrorsSince 从 off 处开始读取新增内容，统计错误行数。
func countErrorsSince(path string, off int64, re *regexp.Regexp) (int, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, "", err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	count := 0
	sample := ""
	for scanner.Scan() {
		line := scanner.Text()
		if isErrorLine(line, re) {
			count++
			if sample == "" {
				if len(line) > 200 {
					sample = line[:200]
				} else {
					sample = line
				}
			}
		}
	}
	return count, sample, scanner.Err()
}

// isErrorLine 判定一行是否为错误日志。
func isErrorLine(line string, re *regexp.Regexp) bool {
	if re != nil {
		return re.MatchString(line)
	}
	upper := strings.ToUpper(line)
	for _, lvl := range []string{"[EMERG]", "[ALERT]", "[CRIT]", "[ERROR]", "EMERG:", "ALERT:", "CRIT:", "ERROR:"} {
		if strings.Contains(upper, lvl) {
			return true
		}
	}
	return false
}
