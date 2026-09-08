// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// JSON 日志中「数值字段」的宽松解析（T060 配套）。
//
// 起因是 nginx 的一个硬约束：`$upstream_addr` / `$upstream_status` /
// `$upstream_response_time` 在没有走 upstream 的请求上（静态文件、redirect、
// error_page、被 deny 拦掉的请求）是**空值**。若 log_format 里把它们写成裸数字
//
//	'"upstream_rt":$upstream_response_time,'
//
// 渲染出来就是 `"upstream_rt":,` —— 非法 JSON，这一整行日志直接报废。因此 T060 下发的
// 格式把**所有**值都加引号，nginx 侧永远合法；代价是数值到了这里变成字符串，
// 于是需要本文件的 flex 类型：数字、带引号数字、空串、"-"、逗号分隔列表（多次
// upstream 尝试，如 "0.002, 0.031"）全部吃得下，且**任何异常都退化为 0 而不报错**
// —— 一个字段解析失败绝不能让整行日志丢失。
package logtail

import (
	"bytes"
	"strconv"
	"strings"
)

// unquoteNum 把 JSON 原始 token 归一化为可解析的数字串：
// 去引号、去空白、取逗号列表首项，空值/占位符返回空串。
func unquoteNum(b []byte) string {
	s := string(bytes.TrimSpace(b))
	if s == "" || s == "null" {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	// 多次 upstream 尝试会产出 "0.002, 0.031" / "502, 200"，取首个（首跳后端）。
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return ""
	}
	return s
}

// flexInt 是容忍字符串形态的整数。
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := unquoteNum(b)
	if s == "" {
		*f = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		*f = 0 // 脏数据不阻断整行解析
		return nil
	}
	*f = flexInt(v)
	return nil
}

// flexUint32 是容忍字符串形态的无符号整数（字节数）。
type flexUint32 uint32

func (f *flexUint32) UnmarshalJSON(b []byte) error {
	s := unquoteNum(b)
	if s == "" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexUint32(v)
	return nil
}

// flexFloat32 是容忍字符串形态的浮点数（耗时）。
type flexFloat32 float32

func (f *flexFloat32) UnmarshalJSON(b []byte) error {
	s := unquoteNum(b)
	if s == "" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 32)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexFloat32(v)
	return nil
}
