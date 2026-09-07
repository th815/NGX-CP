// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package executor 的证书落盘执行器单测（T044）。
package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// certFakeRunner 记录调用并可控 nginx -t / reload 的成败。
type certFakeRunner struct {
	t            *testing.T
	failReload   bool
	reloadCalled bool
}

func (f *certFakeRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	if len(args) >= 1 && args[0] == "-t" {
		return "nginx: configuration file test is successful", nil
	}
	if len(args) >= 2 && args[0] == "-s" && args[1] == "reload" {
		f.reloadCalled = true
		if f.failReload {
			return "", os.ErrInvalid
		}
		return "", nil
	}
	return "", nil
}

func writeOldCert(t *testing.T, sslDir, domain, body string) {
	t.Helper()
	if err := os.MkdirAll(sslDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sslDir, domain+".crt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sslDir, domain+".key"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCertDeploy_Success_PermsAndContent(t *testing.T) {
	dir := t.TempDir()
	sslDir := filepath.Join(dir, "ssl")
	ex := NewCertDeployExecutorWithRunner((&certFakeRunner{t: t}).Output)

	res, err := ex.Deploy(context.Background(), CertDeployRequest{
		Domain:    "example.com",
		CertPEM:   "FAKE-CERT-CHAIN",
		KeyPEM:    "FAKE-KEY",
		SSLDir:    sslDir,
		NginxPath: "/usr/sbin/nginx",
		Reload:    true,
	}, nil)
	if err != nil {
		t.Fatalf("deploy err: %v", err)
	}
	if !res.OK {
		t.Fatalf("期望成功，实际失败: %s", res.Error)
	}

	crt := filepath.Join(sslDir, "example.com.crt")
	key := filepath.Join(sslDir, "example.com.key")
	crtData, err := os.ReadFile(crt)
	if err != nil {
		t.Fatalf("读 crt 失败: %v", err)
	}
	if string(crtData) != "FAKE-CERT-CHAIN" {
		t.Fatalf("crt 内容不符: %q", crtData)
	}
	keyData, err := os.ReadFile(key)
	if err != nil {
		t.Fatalf("读 key 失败: %v", err)
	}
	if string(keyData) != "FAKE-KEY" {
		t.Fatalf("key 内容不符: %q", keyData)
	}

	// 权限：crt 0644 / key 0600。
	ci, _ := os.Stat(crt)
	ki, _ := os.Stat(key)
	if ci.Mode().Perm() != 0o644 {
		t.Fatalf("crt 权限应为 0644，实际 %o", ci.Mode().Perm())
	}
	if ki.Mode().Perm() != 0o600 {
		t.Fatalf("key 权限应为 0600，实际 %o", ki.Mode().Perm())
	}
}

func TestCertDeploy_ReloadFailure_RollbackOldCert(t *testing.T) {
	dir := t.TempDir()
	sslDir := filepath.Join(dir, "ssl")
	// 预置旧证书（被替换的对象）。
	writeOldCert(t, sslDir, "example.com", "OLD-CERT-AND-KEY")

	run := &certFakeRunner{t: t, failReload: true}
	ex := NewCertDeployExecutorWithRunner(run.Output)

	res, err := ex.Deploy(context.Background(), CertDeployRequest{
		Domain:    "example.com",
		CertPEM:   "NEW-CERT",
		KeyPEM:    "NEW-KEY",
		SSLDir:    sslDir,
		NginxPath: "/usr/sbin/nginx",
		Reload:    true,
	}, nil)
	if err != nil {
		t.Fatalf("deploy err: %v", err)
	}
	if res.OK {
		t.Fatalf("期望 reload 失败导致整体失败，实际 OK=true")
	}
	if !run.reloadCalled {
		t.Fatal("期望触发了 reload")
	}

	// 回滚后磁盘应是旧证书内容。
	crtData, err := os.ReadFile(filepath.Join(sslDir, "example.com.crt"))
	if err != nil {
		t.Fatalf("读 crt 失败: %v", err)
	}
	if string(crtData) != "OLD-CERT-AND-KEY" {
		t.Fatalf("回滚未生效，crt 内容=%q，期望旧证书", crtData)
	}
}

func TestCertDeploy_FirstDeployNoOldCert(t *testing.T) {
	dir := t.TempDir()
	sslDir := filepath.Join(dir, "ssl") // 不存在，首次部署
	ex := NewCertDeployExecutorWithRunner((&certFakeRunner{t: t}).Output)

	res, err := ex.Deploy(context.Background(), CertDeployRequest{
		Domain:    "first.example.com",
		CertPEM:   "C",
		KeyPEM:    "K",
		SSLDir:    sslDir,
		NginxPath: "/usr/sbin/nginx",
		Reload:    false,
	}, nil)
	if err != nil {
		t.Fatalf("deploy err: %v", err)
	}
	if !res.OK {
		t.Fatalf("首次部署期望成功，实际: %s", res.Error)
	}
	if _, err := os.Stat(filepath.Join(sslDir, "first.example.com.key")); err != nil {
		t.Fatalf("key 未落盘: %v", err)
	}
}
