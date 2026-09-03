// Package apperr 定义统一的业务错误类型与 API 响应码。
package apperr

import (
	"errors"
	"fmt"
)

// Code 是面向客户端的稳定错误码。
type Code int

const (
	CodeOK          Code = 0
	CodeInvalid     Code = 4001 // 参数非法
	CodeUnauthorized Code = 4003 // 未认证
	CodeForbidden   Code = 4005 // 无权限
	CodeNotFound    Code = 4004
	CodeConflict    Code = 4009 // 并发冲突 / 已存在
	CodePrecondition Code = 4012 // 前置条件不满足（如 nginx -t 失败）
	CodeUnavailable Code = 4103  // 依赖不可用（Agent 离线）
	CodeInternal    Code = 5000
)

// Error 是带码的业务错误。
type Error struct {
	Code    Code
	Message string // 面向用户的中文消息
	Detail  string // 技术细节（命令输出等），输出时会被截断
	Cause   error
}

func New(c Code, msg string) *Error {
	return &Error{Code: c, Message: msg}
}

func Wrap(c Code, msg string, cause error) *Error {
	return &Error{Code: c, Message: msg, Cause: cause}
}

func (e *Error) WithDetail(d string) *Error {
	e.Detail = d
	return e
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%d] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// Unwrap 使 errors.Is / errors.As 可用。
func (e *Error) Unwrap() error { return e.Cause }

// From 将任意 error 归一为 *Error；非 *Error 统一按内部错误。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Wrap(CodeInternal, "服务器内部错误", err)
}

// CodeOf 提取错误码，未识别返回 CodeInternal。
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// IsNotFound 判断是否为"资源不存在"。
func IsNotFound(err error) bool { return CodeOf(err) == CodeNotFound }
