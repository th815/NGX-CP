// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadTestdata 读取 testdata 真实 nginx -t 输出。
func loadTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return string(b)
}

// TestParseValidateOutput 用真实 nginx -t 输出验证解析：
// 正确配置 → OK=true；unknown directive / host not found → OK=false 且 Line 正确；
// bind 端口占用 → OK=false 但归为 warn（运行时问题，非配置错误）。
func TestParseValidateOutput(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		resp := parseValidateOutput(loadTestdata(t, "ok.txt"))
		if !resp.OK {
			t.Fatalf("期望 OK=true，实际 false；raw=%q", resp.Raw)
		}
		if len(resp.Errors) != 0 {
			t.Fatalf("期望无错误，实际 %d 条: %+v", len(resp.Errors), resp.Errors)
		}
	})

	t.Run("unknown_directive", func(t *testing.T) {
		resp := parseValidateOutput(loadTestdata(t, "unknown_directive.txt"))
		if resp.OK {
			t.Fatal("期望 OK=false")
		}
		if len(resp.Errors) != 1 {
			t.Fatalf("期望 1 条错误，实际 %d: %+v", len(resp.Errors), resp.Errors)
		}
		e := resp.Errors[0]
		if e.Level != "emerg" {
			t.Errorf("Level 期望 emerg，实际 %q", e.Level)
		}
		if e.File != "/etc/nginx/conf.d/api.conf" {
			t.Errorf("File 期望 /etc/nginx/conf.d/api.conf，实际 %q", e.File)
		}
		if e.Line != 12 {
			t.Errorf("Line 期望 12，实际 %d", e.Line)
		}
		if e.Message != `unknown directive "lstne"` {
			t.Errorf("Message 不符: %q", e.Message)
		}
	})

	t.Run("host_not_found", func(t *testing.T) {
		resp := parseValidateOutput(loadTestdata(t, "host_not_found.txt"))
		if resp.OK {
			t.Fatal("期望 OK=false")
		}
		if len(resp.Errors) != 1 {
			t.Fatalf("期望 1 条错误，实际 %d: %+v", len(resp.Errors), resp.Errors)
		}
		e := resp.Errors[0]
		if e.Level != "emerg" {
			t.Errorf("Level 期望 emerg，实际 %q", e.Level)
		}
		if e.File != "/etc/nginx/conf.d/api.conf" {
			t.Errorf("File 不符: %q", e.File)
		}
		if e.Line != 5 {
			t.Errorf("Line 期望 5，实际 %d", e.Line)
		}
		if !strings.Contains(e.Message, `host not found in upstream "nonexistent"`) {
			t.Errorf("Message 不符: %q", e.Message)
		}
	})

	t.Run("bind_failed_is_warn", func(t *testing.T) {
		resp := parseValidateOutput(loadTestdata(t, "bind_failed.txt"))
		// bind 端口占用：nginx -t 整体失败，但本质是运行时问题，归为 warn。
		if resp.OK {
			t.Fatal("期望 OK=false（端口被占用导致 test failed）")
		}
		if len(resp.Errors) != 1 {
			t.Fatalf("期望 1 条错误，实际 %d: %+v", len(resp.Errors), resp.Errors)
		}
		e := resp.Errors[0]
		if e.Level != "warn" {
			t.Errorf("端口占用应归为 warn，实际 %q", e.Level)
		}
		if !strings.Contains(e.Message, "Address already in use") {
			t.Errorf("Message 应含 Address already in use: %q", e.Message)
		}
		if e.Line != 0 || e.File != "" {
			t.Errorf("端口占用无 config 文件/行号，期望 Line=0/File=空，实际 %d/%q", e.Line, e.File)
		}
	})
}

// TestValidate 验证完整流程：写 staging 目录 → 调用注入的执行器 → 解析输出。
// 用 fake runner 返回 unknown_directive 的真实输出，确认 OK 与行号透传。
func TestValidate(t *testing.T) {
	// 用系统临时目录作 prefix，验证文件按相对结构写入。
	prefix := t.TempDir()
	req := ValidateRequest{
		NginxPath: "/usr/sbin/nginx",
		Prefix:    prefix,
		ConfPath:  "nginx.conf",
		Files: map[string]string{
			"nginx.conf":      "include conf.d/*.conf; http {}",
			"conf.d/api.conf": `server { lstne 80; }`,
		},
	}
	raw := loadTestdata(t, "unknown_directive.txt")
	ex := NewExecutorWithRunner(func(ctx context.Context, name string, args ...string) (string, error) {
		// 断言命令形态：必须是 -t -p <prefix> -c <prefix>/nginx.conf，绝不能只校验单文件。
		if len(args) < 5 || args[0] != "-t" || args[1] != "-p" || args[3] != "-c" {
			t.Errorf("命令形态不符: %v", args)
		}
		if got := args[2]; got != prefix {
			t.Errorf("-p 期望 %q，实际 %q", prefix, got)
		}
		if got := args[4]; got != filepath.Join(prefix, "nginx.conf") {
			t.Errorf("-c 期望 %q，实际 %q", filepath.Join(prefix, "nginx.conf"), got)
		}
		return raw, nil
	})

	resp, err := ex.Validate(context.Background(), req)
	if err != nil {
		t.Fatalf("Validate 返回错误: %v", err)
	}
	if resp.OK {
		t.Fatal("期望 OK=false")
	}
	if len(resp.Errors) != 1 || resp.Errors[0].Line != 12 {
		t.Fatalf("期望 1 条错误且 Line=12: %+v", resp.Errors)
	}

	// 验证 staging 文件确实写入（且后续被清理由调用方负责；此处仅确认写入发生）。
	if _, err := os.Stat(filepath.Join(prefix, "conf.d", "api.conf")); err != nil {
		t.Errorf("staging 文件未写入: %v", err)
	}
}

// TestValidateNoConfPath 缺主配置路径应报错。
func TestValidateNoConfPath(t *testing.T) {
	ex := NewExecutorWithRunner(func(ctx context.Context, name string, args ...string) (string, error) {
		return "", nil
	})
	if _, err := ex.Validate(context.Background(), ValidateRequest{Files: map[string]string{"nginx.conf": "x"}}); err == nil {
		t.Fatal("期望缺 ConfPath 报错")
	}
}

// BenchmarkParseValidateOutput 校验解析性能（大配置输出 <50ms）。
func BenchmarkParseValidateOutput(b *testing.B) {
	// 构造 ~5000 行错误输出。
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		sb.WriteString("nginx: [emerg] unknown directive \"x\" in /etc/nginx/conf.d/svc.conf:10\n")
	}
	raw := sb.String()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = parseValidateOutput(raw)
	}
}
