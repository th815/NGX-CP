package rules

import (
	"context"
	"os"

	"gopkg.in/yaml.v3"
)

// ConfigFile 是待校验的单个配置文件（含内容）。
type ConfigFile struct {
	Path    string
	Content string
}

// Capability 描述节点的编译能力（模块集合）。
// 生产侧由 capability.NginxInfo 或 ent NodeCapability.modules 适配而来，
// 测试可直接构造。
type Capability struct {
	Modules []string // http_ssl, stream, nginx_upstream_check_module ...
}

// HasModule 判断能力集是否包含某模块（归一化名，如 "http_ssl"）。
func (c *Capability) HasModule(name string) bool {
	if c == nil {
		return false
	}
	for _, m := range c.Modules {
		if m == name {
			return true
		}
	}
	return false
}

// Node 是节点标识，用于 issue 信息定位。
type Node struct {
	ID   int
	Name string
	Role string
}

// Peer 是集群内的同类节点，用于「双机一致性」比对。
type Peer struct {
	Name string
	Role string
	Cap  *Capability
}

// CheckInput 是语义校验的输入。
type CheckInput struct {
	ConfigFiles []ConfigFile
	Capability  *Capability
	Node        *Node
	Peers       []Peer          // 同类节点能力，用于漂移检测
	KnownFiles  []string        // 节点上已知存在的文件路径（证书引用检查用）
}

// Issue 是规则检出的一条问题。
type Issue struct {
	RuleID   string `json:"rule_id"`
	Severity string `json:"severity"` // error | warning | info
	Message  string `json:"message"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

// Rule 是所有语义校验规则的统一接口。
type Rule interface {
	ID() string
	Name() string
	Severity() string
	Enabled() bool
	SetEnabled(bool)
	Check(ctx context.Context, in *CheckInput) []Issue
}

// Config 是 rules.yaml 的运行时表示。
type Config struct {
	// ModuleRequirements 指令 → 需要的编译模块（与 capability 模块名一致）。
	ModuleRequirements map[string]string `yaml:"module_requirements"`
	// RuleToggles 规则 ID → 是否启用（覆盖默认）。
	RuleToggles map[string]bool `yaml:"rules"`
	// Security 安全类规则的可配置阈值。
	Security SecurityConfig `yaml:"security"`
	// DR 配置（DR 模式相关规则用）。
	DR DRConfig `yaml:"dr"`
	// Upstream 可达性探测开关。
	Upstream UpstreamConfig `yaml:"upstream"`
}

// SecurityConfig 安全规则阈值。
type SecurityConfig struct {
	RequireServerTokensOff bool     `yaml:"require_server_tokens_off"`
	MinTLS                 string   `yaml:"min_tls"` // 允许的最低 TLS 版本，如 TLSv1.2
	ForbidDotGitLocation   bool     `yaml:"forbid_dot_git"`
	ForbiddenPaths         []string `yaml:"forbidden_paths"`
}

// DRConfig DR 模式相关配置。
type DRConfig struct {
	VIPs []string `yaml:"vips"` // 集群 VIP 列表（RS 不应直接 listen 这些地址）
}

// UpstreamConfig upstream 可达性探测开关。
type UpstreamConfig struct {
	ResolveDNS bool `yaml:"resolve_dns"` // 是否做 DNS 解析探测（默认关闭，避免测试/网络抖动）
}

// DefaultConfig 返回内建默认配置（无需读取文件即可工作）。
func DefaultConfig() *Config {
	return &Config{
		ModuleRequirements: defaultModuleReqs(),
		RuleToggles:        map[string]bool{},
		Security: SecurityConfig{
			RequireServerTokensOff: true,
			MinTLS:                 "TLSv1.2",
			ForbidDotGitLocation:   true,
			ForbiddenPaths:         []string{".git", ".env", ".sql", ".bak"},
		},
		DR:       DRConfig{VIPs: []string{}},
		Upstream: UpstreamConfig{ResolveDNS: false},
	}
}

func defaultModuleReqs() map[string]string {
	return map[string]string{
		"check":                "nginx_upstream_check_module",
		"ssl_certificate":      "http_ssl",
		"ssl_certificate_key":  "http_ssl",
		"ssl":                  "http_ssl",
		"http2":                "http_v2",
		"http3":                "http_v3",
		"quic":                 "http_v3",
		"brotli":               "ngx_brotli",
		"brotli_static":        "ngx_brotli",
		"real_ip_header":       "http_realip",
		"set_real_ip_from":     "http_realip",
		"stub_status":          "http_stub_status",
		"js_import":            "njs",
		"js_content":           "njs",
		"js_set":               "njs",
		"grpc_pass":            "http_grpc",
		"preread":              "stream_ssl_preread",
		"ssl_preread":          "stream_ssl_preread",
		"v2ray":                "v2ray",
	}
}

// LoadConfig 从 YAML 文件加载规则配置；文件不存在则回退到 DefaultConfig。
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.ModuleRequirements == nil {
		cfg.ModuleRequirements = defaultModuleReqs()
	}
	return cfg, nil
}

// Engine 执行规则集。
type Engine struct {
	rules []Rule
}

// NewEngine 构造引擎：规则默认全开，cfg 中的 RuleToggles 覆盖开关。
func NewEngine(cfg *Config) *Engine {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	e := &Engine{}
	e.rules = defaultRules(cfg)
	for _, r := range e.rules {
		if v, ok := cfg.RuleToggles[r.ID()]; ok {
			r.SetEnabled(v)
		} else {
			r.SetEnabled(true)
		}
	}
	return e
}

// Check 运行所有启用的规则，聚合 issue。
func (e *Engine) Check(ctx context.Context, in *CheckInput) []Issue {
	var issues []Issue
	for _, r := range e.rules {
		if !r.Enabled() {
			continue
		}
		issues = append(issues, r.Check(ctx, in)...)
	}
	return issues
}

// Rules 返回当前规则集（便于测试/调试）。
func (e *Engine) Rules() []Rule { return e.rules }

// defaultRules 构造所有规则实例，绑定同一份 cfg。
func defaultRules(cfg *Config) []Rule {
	return []Rule{
		&ModuleCheckRule{cfg: cfg},
		&CertRefRule{cfg: cfg},
		&UpstreamReachRule{cfg: cfg},
		&PortConflictRule{cfg: cfg},
		&StreamBlockRule{cfg: cfg},
		&DRPortRule{cfg: cfg},
		&SecurityRule{cfg: cfg},
	}
}
