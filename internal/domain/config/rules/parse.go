// Package rules 实现配置语义校验规则引擎。
//
// 与 T024 的 `nginx -t`（硬语法门禁）互补：nginx -t 已能捕获大部分「未知指令 /
// 语法错误」，但无法发现「配置用到了节点没编译的模块（双机编译不一致）」「引用了
// 不存在的证书」「upstream 结构残缺」「端口冲突」「DR 模式下误绑 VIP」等平台语义问题。
// 这些规则可配置开关（configs/rules.yaml），每条都给出 Fix 修复建议。
package rules

import (
	"strings"
)

// stmt 是一条被解析出的配置语句（指令 + 参数 + 绝对行号）。
type stmt struct {
	name string   // 指令名（首 token）
	args []string // 其余参数
	line int      // 1-based 行号
}

// splitArgs 按空白切分一行指令，尊重单引号内空格（如 '1.2.3'）。
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
		case r == ' ' || r == '\t':
			if !inQuote {
				if cur.Len() > 0 {
					args = append(args, cur.String())
					cur.Reset()
				}
				continue
			}
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

// statements 将配置原文解析为扁平语句流，兼容单行块与跨行块。
// 以 ';' 结束一条语句，'{'/'}' 视为块边界（先 flush 当前语句再处理边界）。
func statements(content string) []stmt {
	var out []stmt
	var buf strings.Builder
	line := 1
	curLine := 1
	flush := func() {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		if s == "" {
			return
		}
		s = strings.Trim(s, "{}")
		s = strings.TrimRight(s, ";") // `;` 是语句终结符，不应进入参数
		s = strings.TrimSpace(s)
		if i := strings.Index(s, "#"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		if s == "" {
			return
		}
		a := splitArgs(s)
		if len(a) == 0 {
			return
		}
		out = append(out, stmt{name: a[0], args: a[1:], line: curLine})
	}
	for _, r := range content {
		switch r {
		case '\n':
			line++
			curLine = line
		case ';':
			buf.WriteRune(r)
			curLine = line
			flush()
		case '{', '}':
			flush()
			curLine = line
		default:
			buf.WriteRune(r)
		}
	}
	flush()
	return out
}

// allDirectives 返回配置中出现过的指令名集合（去重）。
func allDirectives(content string) map[string]bool {
	set := map[string]bool{}
	for _, s := range statements(content) {
		set[s.name] = true
	}
	return set
}

// firstLineOf 返回某指令首次出现的 1-based 行号，未出现返回 0。
func firstLineOf(content, directive string) int {
	for _, s := range statements(content) {
		if s.name == directive {
			return s.line
		}
	}
	return 0
}

// block 是一段用花括号包裹的配置块（server/http/upstream/stream 等），行号为 1-based。
type block struct {
	Start int
	End   int
}

// blocks 返回所有以 keyword 开头的块的行号范围（1-based，含首尾花括号所在行）。
// 通过花括号深度计数正确处理嵌套。
func blocks(content, keyword string) []block {
	lines := strings.Split(content, "\n")
	var res []block
	for i := 0; i < len(lines); i++ {
		if !isBlockOpen(lines[i], keyword) {
			continue
		}
		depth := strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		start := i
		j := i
		for j < len(lines) && depth > 0 {
			j++
			if j < len(lines) {
				t := strings.TrimSpace(lines[j])
				if strings.HasPrefix(t, "#") {
					continue
				}
				depth += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
			}
		}
		res = append(res, block{Start: start + 1, End: j + 1})
		i = j
	}
	return res
}

// stmtsInBlock 返回落在指定块行号范围内的语句（含块首行 keyword 语句本身）。
func stmtsInBlock(content string, b block) []stmt {
	var out []stmt
	for _, s := range statements(content) {
		if s.line >= b.Start && s.line <= b.End {
			out = append(out, s)
		}
	}
	return out
}

// isBlockOpen 判断一行是否以 `keyword [name] {` 形式开启一个块。
func isBlockOpen(line, keyword string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, keyword) {
		return false
	}
	rest := s[len(keyword):]
	if len(rest) == 0 {
		return false
	}
	if rest[0] != ' ' && rest[0] != '\t' && rest[0] != '{' {
		return false
	}
	return strings.Contains(s, "{")
}

// stmtsOf 返回整个配置的语句流（便捷封装）。
func stmtsOf(content string) []stmt { return statements(content) }
