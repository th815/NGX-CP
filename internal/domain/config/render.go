// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package config

import (
	"bytes"
	"fmt"
	"regexp"
	"text/template"
)

// actionRe 匹配模板动作 {{ ... }}，用于把变量引用限定在动作内部，
// 避免把配置文本里 example.com 这种字面量误判为变量引用。
var actionRe = regexp.MustCompile(`{{.*?}}`)

// refRe 匹配动作内的变量引用：.ident 或 $.ident（支持 $ 与 . 间空白）。
var refRe = regexp.MustCompile(`(?:\$\s*\.)?\s*\.([A-Za-z_][A-Za-z0-9_]*)`)

// extractRefs 提取模板里引用的全部变量名（去重）。
// 仅扫描 {{ }} 动作内部，命中 .ident / $.ident 两种写法。
func extractRefs(content string) []string {
	seen := make(map[string]bool)
	var refs []string
	for _, act := range actionRe.FindAllString(content, -1) {
		// 去掉首尾 {{ 与 }}（已确认动作长度 >= 4）。
		inner := act[2 : len(act)-2]
		for _, m := range refRe.FindAllStringSubmatch(inner, -1) {
			name := m[1]
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			refs = append(refs, name)
		}
	}
	return refs
}

// Render 用 Go template 渲染 content，变量取自 vars（key→value 的扁平映射）。
//
// 两条护栏（对应 T027 契约陷阱）：
//  1. 缺失变量必须报错且明确指出缺哪个——先用 extractRefs 预检，命中缺失即返回
//     形如 "模板缺少变量 \"missing\"" 的错误；再用 missingkey=error 兜底（任何漏网引用也会在
//     执行期报错，而非渲染成空串导致 nginx 静默错误）。
//  2. 模板语法错误在 Parse 阶段即返回错误。
func Render(content string, vars map[string]string) (string, error) {
	for _, name := range extractRefs(content) {
		if _, ok := vars[name]; !ok {
			return "", fmt.Errorf("模板缺少变量 %q", name)
		}
	}

	tmpl, err := template.New("cfg").
		Option("missingkey=error").
		Parse(content)
	if err != nil {
		return "", fmt.Errorf("解析模板失败: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("渲染模板失败: %w", err)
	}
	return buf.String(), nil
}
