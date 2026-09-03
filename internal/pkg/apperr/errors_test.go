package apperr

import (
	"errors"
	"testing"
)

func TestWrapUnwrap(t *testing.T) {
	root := errors.New("connection refused")
	e := Wrap(CodeUnavailable, "Agent 离线", root)
	if !errors.Is(e, root) {
		t.Error("Unwrap 失败：errors.Is 应匹配 root")
	}
	if e.Code != CodeUnavailable {
		t.Errorf("码错误: %v", e.Code)
	}
}

func TestCodeOfDowngrade(t *testing.T) {
	if CodeOf(errors.New("random")) != CodeInternal {
		t.Error("未知错误应降级为 CodeInternal")
	}
	if CodeOf(nil) != CodeInternal {
		t.Error("nil 不应被判为 NotFound/其它")
	}
}

func TestFromNormalizes(t *testing.T) {
	e := From(errors.New("boom"))
	if e.Code != CodeInternal {
		t.Errorf("From 归一失败: %v", e.Code)
	}
	wrapped := Wrap(CodeInvalid, "bad", nil)
	if From(wrapped) != wrapped {
		t.Error("已是 *Error 时 From 应原样返回")
	}
}

func TestWithDetail(t *testing.T) {
	e := New(CodePrecondition, "nginx -t 失败").WithDetail("unknown directive")
	if e.Detail == "" {
		t.Error("WithDetail 未生效")
	}
}
