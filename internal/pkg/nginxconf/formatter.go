// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package nginxconf 提供 nginx 配置指令的词法级解析能力（自实现，无第三方依赖）。
//
// 本文件落地 T022 的「语义对齐」：把不同缩进风格的同一配置规范化为统一缩进，
// 使纯缩进/多余空白差异在 diff 时不再产生噪音（见 diff.go 的 DiffNginx）。
package nginxconf

import "strings"

// Format 把 nginx 配置规范化为统一缩进（每级 4 空格），消除纯缩进 / 行尾多余空白造成的 diff 噪音。
//
// 不改动任何指令名、参数、注释内容，仅做空白规范化与花括号层级对齐。两份仅缩进不同的配置
// 经 Format 后逐字节相同，从而实现「缩进变化不进 diff」。
//
// 采用基于花括号计数的启发式：无法正确处理字符串 / 正则中出现的字面量 { }（极罕见），
// 本函数仅用于 diff 去噪，不保证与 nginx 解析器的语义完全一致。
func Format(content string) string {
	var b strings.Builder
	depth := 0
	lines := strings.Split(content, "\n")
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			// 保留空行以维持段落结构（作为 context），但去掉其前导空白。
			b.WriteByte('\n')
			continue
		}
		// 行首若为 '}'（或 '};'），先减一级再缩进，使闭合括号与开括号对齐。
		effDepth := depth
		if strings.HasPrefix(trimmed, "}") {
			effDepth--
			if effDepth < 0 {
				effDepth = 0
			}
		}
		b.WriteString(strings.Repeat("    ", effDepth))
		b.WriteString(trimmed)
		b.WriteByte('\n')
		// 按本行花括号净增减更新层级（同时出现的开 / 闭括号相互抵消）。
		open := strings.Count(trimmed, "{")
		closeB := strings.Count(trimmed, "}")
		depth += open - closeB
		if depth < 0 {
			depth = 0
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
