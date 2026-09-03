// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package nginxconf 提供 nginx 配置指令的词法级解析能力（自实现，无第三方依赖）。
//
// nginx 配置语法比看上去宽松：参数之间可以有多余空白，允许单/双引号包裹含空格的
// 参数（如 `access_log "/var/log/nginx/my access.log";`），行尾以分号结束，
// 注释以 `#` 起至行尾。因此不能用简单的 `strings.Fields` + `strings.HasSuffix(";")`
// 处理，否则遇到引号路径或行尾注释就会解析错。
//
// 本包只做**词法切分**，不构建完整语法树——完整 AST 属于配置中心（M2）的职责，
// 这里保持最小可用：给日志目标提取（T018）与后续指令扫描提供可靠的 token 流。
package nginxconf

import "strings"

// SplitArgs 按 nginx 词法规则切分参数列表。
//
// 规则：
//   - 空白（空格/制表符/换行）为分隔符，连续空白视为一个；
//   - 单引号或双引号内的内容视为单一参数（保留内容，去掉引号本身）；
//   - 引号外的 `#` 表示注释开始，其后内容忽略；
//   - 引号外的 `;` 表示指令结束，其后内容不再解析；
//   - 不处理转义（nginx 配置本身不支持反斜杠转义，反斜杠按字面量保留）。
//
// 返回不含分号的参数切片；空输入返回 nil。
func SplitArgs(s string) []string {
	var (
		args   []string
		cur    strings.Builder
		hasCur bool
		quote  byte // 0 表示不在引号内
	)
	flush := func() {
		if hasCur {
			args = append(args, cur.String())
			cur.Reset()
			hasCur = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			// 引号内：只有配对的同型引号才结束，其余一律字面量（含空白、# 与 ;）。
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			hasCur = true
		case c == '"' || c == '\'':
			quote = c
			hasCur = true // 允许空字符串参数 "" 存在
		case c == '#':
			flush()
			i = skipToEOL(s, i) // 注释持续到行尾
		case c == ';':
			flush()
			return args // 指令结束，后续内容不属于本指令
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			flush()
		default:
			cur.WriteByte(c)
			hasCur = true
		}
	}
	flush()
	return args
}

// skipToEOL 返回行尾换行符的下标（供 for 循环的 i++ 抵消），已到末尾则返回 len(s)-1。
func skipToEOL(s string, i int) int {
	for ; i < len(s) && s[i] != '\n'; i++ {
	}
	if i >= len(s) {
		return len(s) - 1
	}
	return i
}

// ScanDirectives 扫描整段 nginx 配置，产出指令 token 流（每条的首元素为指令名）。
//
// 与 SplitArgs 的区别：SplitArgs 处理**单条**指令，ScanDirectives 处理**整份配置**，
// 需要额外处理三件事：
//  1. 块结构 —— `{` 前的累积内容是一条块指令（http / server / location ...），
//     必须在此切分，否则块头会与块内第一条指令粘成一坨，导致块内指令全部丢失；
//  2. `}` 结束块，残留内容一并收尾；
//  3. 指令可跨行书写，`;`（引号外）才是唯一的结束标记。
//
// 注释（`#` 至行尾）、引号含空格路径、未闭合引号均按 nginx 实际行为处理。
// 例：
//
//	ScanDirectives("http {\n access_log /a.log main;\n}")
//	  -> [["http"], ["access_log", "/a.log", "main"]]
func ScanDirectives(content string) [][]string {
	var (
		out    [][]string
		cur    []string
		buf    strings.Builder
		hasBuf bool
		quote  byte
	)
	flushArg := func() {
		if hasBuf {
			cur = append(cur, buf.String())
			buf.Reset()
			hasBuf = false
		}
	}
	// emit 收尾当前指令：{ } ; 与 EOF 都会触发。块指令（http/server/...）也在此产出。
	emit := func() {
		flushArg()
		if len(cur) > 0 {
			out = append(out, cur)
		}
		cur = nil
	}
	for i := 0; i < len(content); i++ {
		c := content[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			// 引号内一律字面量，含换行（nginx 允许未闭合引号跨行，虽罕见但要容错）。
			buf.WriteByte(c)
			hasBuf = true
			continue
		}
		switch c {
		case '#':
			i = skipToEOL(content, i)
		case '"', '\'':
			quote = c
			hasBuf = true
		case '{', '}', ';':
			emit()
		case ' ', '\t', '\r', '\n':
			flushArg()
		default:
			buf.WriteByte(c)
			hasBuf = true
		}
	}
	emit() // 容忍末尾漏写分号/右括号
	return out
}

// StripComment 去掉引号外的 `#` 注释部分。引线内的 `#` 属于参数内容，不截断。
// 无注释时返回原串（不额外分配）。
func StripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return s[:i]
		}
	}
	return s
}

// SplitDirective 把一条指令切分为指令名与参数，并去掉结尾的分号。
//
// 输入可为整行（含分号）或已去掉分号的片段。返回 ok=false 表示没有任何有效 token
// （空行或纯注释），调用方应跳过。
//
// 例：
//
//	`access_log /var/log/nginx/access.log main buffer=32k;` ->
//	    "access_log", [".../access.log", "main", "buffer=32k"]
//	`error_log  /var/log/nginx/error.log  warn;` ->
//	    "error_log", [".../error.log", "warn"]
func SplitDirective(s string) (name string, args []string, ok bool) {
	// SplitArgs 已处理注释截断与分号终止，无需在此二次裁剪。
	args = SplitArgs(s)
	if len(args) == 0 {
		return "", nil, false
	}
	return args[0], args[1:], true
}

// IsDirectiveComplete 判断一段指令文本是否已以分号结束（引号外的分号才算）。
// nginx 允许指令跨行书写，扫描配置时需要据此判断是否需要继续累积下一行。
func IsDirectiveComplete(s string) bool {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ';':
			return true
		}
	}
	return false
}
