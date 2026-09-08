// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// 带块作用域的指令扫描（T060 引入）。
//
// ScanDirectives 只产出扁平 token 流，够 T018 提取日志目标用，但两类判断必须知道
// 指令**在哪个块里**：
//   - `include conf.d/*.conf;` 在 http{} 内才能承载 log_format/access_log；
//     若它在 main 层级（有些配置把 stream 的 include 放在最外层），下发片段会让
//     `nginx -t` 直接报 "log_format directive is not allowed here"；
//   - `access_log` 在 server{}/location{} 内会**覆盖**父层级设置（nginx 的
//     access_log 不是叠加继承而是就近覆盖），这些站点不会写入 http 级新增的
//     JSON 日志，必须显式告警而不是让用户以为下发成功。
//
// 仍然只做词法级扫描（不建 AST），复用与 ScanDirectives 完全一致的引号/注释规则，
// 避免两套实现行为漂移。
package nginxconf

import "strings"

// ScopedDirective 是一条带块路径的指令。
//
// Scope 自外向内记录祖先块头（含参数），例如 location 块记为 "location /api"。
// 顶层指令的 Scope 为空切片。
type ScopedDirective struct {
	Scope []string // ["http"] / ["http","server"] / ["http","server","location /api"]
	Name  string   // 指令名，如 access_log
	Args  []string // 参数（不含指令名与分号）
}

// InScope 判断指令是否位于名为 block 的块内（按块头首 token 比较，任意深度）。
func (d ScopedDirective) InScope(block string) bool {
	for _, s := range d.Scope {
		if s == block || strings.HasPrefix(s, block+" ") {
			return true
		}
	}
	return false
}

// Depth 返回指令所在的块层数（顶层为 0）。
func (d ScopedDirective) Depth() int { return len(d.Scope) }

// ScanScoped 扫描整份配置，产出带块作用域的指令流。
//
// 与 ScanDirectives 的差异：
//   - `{` 之前累积的 token 视为块头，压入作用域栈（不作为普通指令产出）；
//   - `}` 弹出作用域栈，并容忍块内最后一条指令漏写分号；
//   - 花括号不配平时（配置损坏）不 panic：多余的 `}` 忽略，未闭合的块在 EOF 收尾。
//
// 例：
//
//	ScanScoped("http {\n server {\n access_log /a.log main;\n }\n}")
//	  -> [{Scope:["http","server"], Name:"access_log", Args:["/a.log","main"]}]
func ScanScoped(content string) []ScopedDirective {
	var (
		out    []ScopedDirective
		stack  []string
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
	emit := func() {
		flushArg()
		if len(cur) == 0 {
			return
		}
		// Scope 必须拷贝：stack 底层数组会被后续 append/截断复用。
		scope := make([]string, len(stack))
		copy(scope, stack)
		out = append(out, ScopedDirective{Scope: scope, Name: cur[0], Args: cur[1:]})
		cur = nil
	}
	for i := 0; i < len(content); i++ {
		c := content[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
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
		case '{':
			flushArg()
			stack = append(stack, strings.Join(cur, " ")) // 匿名块（cur 为空）压入 "" 保持配平
			cur = nil
		case '}':
			emit() // 容忍块内最后一条指令漏写分号
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case ';':
			emit()
		case ' ', '\t', '\r', '\n':
			flushArg()
		default:
			buf.WriteByte(c)
			hasBuf = true
		}
	}
	emit()
	return out
}
