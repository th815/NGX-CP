// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package config 实现配置中心的核心领域逻辑。本文件落地 T022：任意两版配置的 Diff 计算。
//
// 设计要点：
//   - Diff 用 Myers 算法（github.com/hexops/gotextdiff，与 git / gopls 同款），纯文本逐行比对；
//   - DiffNginx 先做语义对齐（nginxconf.Format 统一缩进）再比对，消除纯缩进 / 多余空白噪音；
//   - DiffResult / Hunk / DiffLine 结构贴合前端并排 diff 渲染（T023 / T028）。
package config

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/internal/pkg/nginxconf"
)

// isNotFound 封装 ent 的未找到判断，简化调用点。
func isNotFound(err error) bool { return ent.IsNotFound(err) }

// DiffResult 是两版配置的差异结果。
type DiffResult struct {
	OldRev int
	NewRev int
	Hunks  []Hunk
	Stats  DiffStats
}

// Hunk 是一个连续差异区块（对应 unified diff 的 @@ ... @@）。
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Lines              []DiffLine
}

// DiffLine 是 Hunk 内的一行，Type 为 "add" | "del" | "context"。
// OldNo/NewNo 为 0 表示该行在对应版本中不存在（新增行无 OldNo，删除行无 NewNo）。
type DiffLine struct {
	Type    string
	Content string
	OldNo   int
	NewNo   int
}

// DiffStats 汇总增删改行数。
type DiffStats struct {
	Added, Deleted, Changed int
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// Diff 纯文本 Myers diff（与 git 同算法），不消除缩进噪音。
func Diff(oldContent, newContent string) *DiffResult {
	return computeDiff(oldContent, newContent, "old", "new", 0, 0)
}

// DiffNginx 先做语义对齐（统一缩进）再 diff，消除纯缩进 / 空白变化造成的噪音。
// 指令、参数、注释内容保持不变，仅空白规范化。
func DiffNginx(oldContent, newContent string) *DiffResult {
	return computeDiff(nginxconf.Format(oldContent), nginxconf.Format(newContent), "old", "new", 0, 0)
}

// computeDiff 运行 Myers 算法并解析为 DiffResult。
func computeDiff(oldContent, newContent, oldLabel, newLabel string, oldRev, newRev int) *DiffResult {
	edits := myers.ComputeEdits(span.URIFromPath(oldLabel), oldContent, newContent)
	unified := fmt.Sprint(gotextdiff.ToUnified(oldLabel, newLabel, oldContent, edits))
	return parseUnified(unified, oldRev, newRev)
}

// parseUnified 把 gotextdiff 产出的 unified diff 文本解析为结构化 DiffResult。
// 兼容 hunk 头带 / 不带逗号的两种计数写法（如 @@ -1,3 +1,4 @@ 与 @@ -1 +1 @@）。
func parseUnified(text string, oldRev, newRev int) *DiffResult {
	res := &DiffResult{OldRev: oldRev, NewRev: newRev}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		m := hunkHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		oldStart := atoiSafe(m[1])
		oldCount := atoiSafeDefault(m[2], 1)
		newStart := atoiSafe(m[3])
		newCount := atoiSafeDefault(m[4], 1)

		hunk := Hunk{OldStart: oldStart, OldLines: oldCount, NewStart: newStart, NewLines: newCount}
		oldNo := oldStart
		newNo := newStart
		// 消费该 hunk 的内容行，直到下一个 hunk 头或文件结束。
		i++
		for ; i < len(lines); i++ {
			cl := lines[i]
			if cl == "" {
				continue // 跳过 split 产生的尾部空行
			}
			if strings.HasPrefix(cl, "@@") {
				i-- // 交还给外层循环处理
				break
			}
			if strings.HasPrefix(cl, "\\") {
				continue // "\ No newline at end of file" 等元信息，跳过
			}
			var dl DiffLine
			switch cl[0] {
			case ' ':
				dl = DiffLine{Type: "context", Content: cl[1:], OldNo: oldNo, NewNo: newNo}
				oldNo++
				newNo++
			case '-':
				dl = DiffLine{Type: "del", Content: cl[1:], OldNo: oldNo, NewNo: 0}
				oldNo++
				res.Stats.Deleted++
			case '+':
				dl = DiffLine{Type: "add", Content: cl[1:], OldNo: 0, NewNo: newNo}
				newNo++
				res.Stats.Added++
			default:
				// 未知前缀（理论上不应出现），按 context 兜底。
				dl = DiffLine{Type: "context", Content: cl, OldNo: oldNo, NewNo: newNo}
				oldNo++
				newNo++
			}
			hunk.Lines = append(hunk.Lines, dl)
		}
		res.Hunks = append(res.Hunks, hunk)
	}
	// Changed 取增删较小值：修改一行 = 1 删 + 1 增，记为 1 处变更。
	res.Stats.Changed = minInt(res.Stats.Added, res.Stats.Deleted)
	return res
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func atoiSafeDefault(s string, def int) int {
	if s == "" {
		return def
	}
	return atoiSafe(s)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// DiffRevisions 对同文件的任意两版做语义 diff（带版本号标注，供 API 使用）。
// oldRev / newRev 为 config_revision 主键；返回 nil 表示两版内容完全一致。
func (s *ConfigStore) DiffRevisions(ctx context.Context, fileID, oldRev, newRev int) (*DiffResult, error) {
	if _, err := s.client.ConfigFile.Get(ctx, fileID); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("config file %d 不存在", fileID)
		}
		return nil, fmt.Errorf("get config file: %w", err)
	}
	oldC, err := s.revisionContent(ctx, oldRev)
	if err != nil {
		return nil, err
	}
	newC, err := s.revisionContent(ctx, newRev)
	if err != nil {
		return nil, err
	}
	res := DiffNginx(oldC, newC)
	res.OldRev = oldRev
	res.NewRev = newRev
	return res, nil
}

// revisionContent 取某版本号的 blob 内容。
func (s *ConfigStore) revisionContent(ctx context.Context, revID int) (string, error) {
	rev, err := s.client.ConfigRevision.Get(ctx, revID)
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("revision %d 不存在", revID)
		}
		return "", fmt.Errorf("get revision: %w", err)
	}
	b, err := rev.QueryBlob().Only(ctx)
	if err != nil {
		return "", fmt.Errorf("query blob of revision %d: %w", revID, err)
	}
	return b.Content, nil
}
