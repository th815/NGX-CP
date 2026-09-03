package rules

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hasIssue(issues []Issue, ruleID, sev string) *Issue {
	for i := range issues {
		if issues[i].RuleID == ruleID && (sev == "" || issues[i].Severity == sev) {
			return &issues[i]
		}
	}
	return nil
}

func countIssues(issues []Issue, ruleID, sev string) int {
	n := 0
	for i := range issues {
		if issues[i].RuleID == ruleID && (sev == "" || issues[i].Severity == sev) {
			n++
		}
	}
	return n
}

func run(engine *Engine, in *CheckInput) []Issue {
	return engine.Check(context.Background(), in)
}

const nginxConf = "/etc/nginx/nginx.conf"

// ---------------- ModuleCheckRule ----------------

func TestModuleCheck_Pass(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "upstream u { server 1.1.1.1; check interval=3000; }"}},
		Capability:  &Capability{Modules: []string{"nginx_upstream_check_module"}},
	}
	issues := run(eng, in)
	assert.Nil(t, hasIssue(issues, "module_check", "error"), "节点已编译该模块不应报错")
}

func TestModuleCheck_FailAndDrift(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		Node:        &Node{Name: "rs-01", Role: "real_server"},
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "upstream u { server 1.1.1.1; check interval=3000; }"}},
		Capability:  &Capability{Modules: []string{"http_ssl"}},
		Peers:       []Peer{{Name: "rs-02", Role: "real_server", Cap: &Capability{Modules: []string{"nginx_upstream_check_module"}}}},
	}
	issues := run(eng, in)
	err := hasIssue(issues, "module_check", "error")
	require.NotNil(t, err, "节点缺模块应报 error")
	assert.Contains(t, err.Message, "nginx_upstream_check_module")
	require.NotNil(t, hasIssue(issues, "module_check", "warning"), "同类节点有该模块应额外告警漂移")
}

// ---------------- CertRefRule ----------------

func TestCertRef_Pass(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "ssl_certificate /etc/nginx/ssl/full.pem;"}},
		KnownFiles:  []string{"/etc/nginx/ssl/full.pem"},
	}
	assert.Nil(t, hasIssue(run(eng, in), "cert_ref", "error"))
}

func TestCertRef_Fail(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "ssl_certificate /etc/nginx/ssl/missing.pem;"}},
		KnownFiles:  []string{"/etc/nginx/ssl/full.pem"},
	}
	err := hasIssue(run(eng, in), "cert_ref", "error")
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "missing.pem")
}

// ---------------- UpstreamReachRule ----------------

func TestUpstream_Pass(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "upstream u { server 1.1.1.1:80; }\nserver { location / { proxy_pass http://u; } }"}},
	}
	assert.Nil(t, hasIssue(run(eng, in), "upstream_reach", "error"))
}

func TestUpstream_FailEmptyAndUndeclared(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "upstream u { }\nserver { location / { proxy_pass http://ghost; } }"}},
	}
	issues := run(eng, in)
	require.NotNil(t, hasIssue(issues, "upstream_reach", "error"))
	// 空 upstream + 未声明引用，至少两条 error
	assert.GreaterOrEqual(t, countIssues(issues, "upstream_reach", "error"), 2)
}

// ---------------- PortConflictRule ----------------

func TestPortConflict_Pass(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "server { listen 80; }\nserver { listen 443 ssl; }"}},
	}
	assert.Nil(t, hasIssue(run(eng, in), "port_conflict", "error"))
}

func TestPortConflict_Fail(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "server { listen 80; }\nserver { listen 80; }"}},
	}
	err := hasIssue(run(eng, in), "port_conflict", "error")
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "重复监听")
}

// ---------------- StreamBlockRule ----------------

func TestStream_Pass(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "stream { server { listen 53 udp; } }"}},
		Capability:  &Capability{Modules: []string{"stream"}},
	}
	assert.Nil(t, hasIssue(run(eng, in), "stream_block", "error"))
}

func TestStream_Fail(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "stream { server { listen 53 udp; } }"}},
		Capability:  &Capability{Modules: []string{}},
	}
	err := hasIssue(run(eng, in), "stream_block", "error")
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "--with-stream")
}

// ---------------- DRPortRule ----------------

func TestDR_Pass(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DR.VIPs = []string{"10.0.0.100"}
	eng := NewEngine(cfg)
	in := &CheckInput{
		Node:        &Node{Role: "real_server"},
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "server { listen 0.0.0.0:80; }"}},
	}
	assert.Nil(t, hasIssue(run(eng, in), "dr_port", "warning"))
}

func TestDR_Fail(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DR.VIPs = []string{"10.0.0.100"}
	eng := NewEngine(cfg)
	in := &CheckInput{
		Node:        &Node{Role: "real_server"},
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "server { listen 10.0.0.100:80; }"}},
	}
	w := hasIssue(run(eng, in), "dr_port", "warning")
	require.NotNil(t, w)
	assert.Contains(t, w.Message, "VIP")
}

// ---------------- SecurityRule ----------------

func TestSecurity_Pass(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "server { server_tokens off; listen 443 ssl; ssl_protocols TLSv1.2 TLSv1.3; }"}},
	}
	assert.Nil(t, hasIssue(run(eng, in), "security", "warning"))
}

func TestSecurity_Fail(t *testing.T) {
	eng := NewEngine(DefaultConfig())
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "server { server_tokens on; listen 443 ssl; location /.git/ { } }"}},
	}
	issues := run(eng, in)
	require.NotNil(t, hasIssue(issues, "security", "warning"))
	assert.NotNil(t, hasIssue(issues, "security", "warning"))
	// server_tokens on + 暴露 .git 各一条
	assert.GreaterOrEqual(t, countIssues(issues, "security", "warning"), 2)
}

// ---------------- Config / Loader ----------------

func TestLoadConfig(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))))
	cfg, err := LoadConfig(filepath.Join(root, "configs", "rules.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "nginx_upstream_check_module", cfg.ModuleRequirements["check"])
	assert.True(t, cfg.Security.RequireServerTokensOff)
	assert.Equal(t, "TLSv1.2", cfg.Security.MinTLS)

	eng := NewEngine(cfg)
	r := eng.Rules()
	require.Len(t, r, 7)
	// 全部默认启用
	for _, ru := range r {
		assert.True(t, ru.Enabled(), ru.ID()+" 应默认启用")
	}
}

func TestEngine_DisableRule(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RuleToggles["security"] = false
	eng := NewEngine(cfg)
	for _, ru := range eng.Rules() {
		if ru.ID() == "security" {
			assert.False(t, ru.Enabled())
		}
	}
	in := &CheckInput{
		ConfigFiles: []ConfigFile{{Path: nginxConf, Content: "server { server_tokens on; }"}},
	}
	assert.Nil(t, hasIssue(run(eng, in), "security", ""), "关闭后不应再检出处")
}
