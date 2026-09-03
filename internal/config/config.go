// Package config 负责加载与校验控制面配置：文件(yaml) + 环境变量(NGXCP_ 前缀) 覆盖。
//
// 注意：采用「扁平顶层 key」而非嵌套结构，是为了让环境变量名与任务文档完全一致
// （NGXCP_DB_DRIVER / NGXCP_LISTEN / NGXCP_LOG_LEVEL …）。若用 viper 嵌套结构，
// 环境变量会变成 NGXCP_DATABASE_DRIVER，与文档约定不符，易踩坑。
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 是控制面的完整配置。所有字段均为顶层 key，对应 NGXCP_<KEY> 环境变量。
type Config struct {
	Listen    string `mapstructure:"listen"`
	AgentGRPC string `mapstructure:"agent_grpc"`
	BaseURL   string `mapstructure:"base_url"`

	DBDriver   string `mapstructure:"db_driver"`
	DBDsn      string `mapstructure:"db_dsn"`
	DBMaxOpen  int    `mapstructure:"db_max_open_conns"`
	DBMaxIdle  int    `mapstructure:"db_max_idle_conns"`

	PKICACert  string        `mapstructure:"pki_ca_cert"`
	PKICAKey   string        `mapstructure:"pki_ca_key"`
	PKIAgentTTL time.Duration `mapstructure:"pki_agent_cert_ttl"`

	SnapDir     string `mapstructure:"storage_snapshots_dir"`
	ArtifactDir string `mapstructure:"storage_artifacts_dir"`

	CHDsn     string `mapstructure:"clickhouse_dsn"`
	CHMaxMem  string `mapstructure:"clickhouse_max_memory_usage"`
	CHTTLDays int    `mapstructure:"clickhouse_ttl_days"`

	VMEnabled bool   `mapstructure:"victoria_metrics_enabled"`
	VMURL     string `mapstructure:"victoria_metrics_url"`

	LogLevel  string `mapstructure:"log_level"`
	LogPretty bool   `mapstructure:"log_pretty"`

	TOTPRequired  bool   `mapstructure:"security_totp_required"`
	SessionSecret string `mapstructure:"security_session_secret"`

	// AuthAdminToken 是 M1 最小可用鉴权：Bearer 令牌（本地账号 + 多角色留到 M9）。
	// 留空 = 禁用所有写接口（返回 401）；开发态在 config 里填一个值即可。
	AuthAdminToken string `mapstructure:"auth_admin_token"`
	// DBAutoMigrate 开发态自动建表；生产置 false 并改用 make migrate-dev。
	DBAutoMigrate bool `mapstructure:"db_auto_migrate"`
}

// Load 读取配置文件（可选）并叠加环境变量覆盖。环境变量前缀 NGXCP_，. 替换为 _。
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	v.SetEnvPrefix("NGXCP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	v.SetDefault("listen", ":8080")
	v.SetDefault("agent_grpc", ":9443")
	v.SetDefault("base_url", "http://127.0.0.1:8080")
	v.SetDefault("log_level", "info")
	v.SetDefault("log_pretty", false)
	v.SetDefault("db_driver", "sqlite")
	v.SetDefault("db_dsn", "file:./dev.db?cache=shared&_fk=1")
	v.SetDefault("db_max_open_conns", 20)
	v.SetDefault("db_max_idle_conns", 10)
	v.SetDefault("pki_agent_cert_ttl", "8760h")
	v.SetDefault("clickhouse_max_memory_usage", "6G")
	v.SetDefault("clickhouse_ttl_days", 7)
	v.SetDefault("storage_snapshots_dir", "/var/lib/ngxcp/snapshots")
	v.SetDefault("storage_artifacts_dir", "/var/lib/ngxcp/artifacts")
	v.SetDefault("auth_admin_token", "") // 留空 = 禁用写接口；开发态在 config 里填值
	v.SetDefault("db_auto_migrate", true) // M1 开发态自动建表；生产置 false 并改用 make migrate-dev

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &cfg, nil
}

// Validate 做基础一致性校验。
func (c *Config) Validate() error {
	switch c.DBDriver {
	case "postgres", "sqlite":
	default:
		return fmt.Errorf("db_driver 非法: %q (期望 postgres|sqlite)", c.DBDriver)
	}
	if c.DBDsn == "" {
		return fmt.Errorf("db_dsn 不能为空")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level 非法: %q", c.LogLevel)
	}
	if c.Listen == "" {
		return fmt.Errorf("listen 不能为空")
	}
	return nil
}

// String 返回脱敏后的配置摘要（DSN 密码被遮罩），用于 --check-config 与日志。
func (c *Config) String() string {
	return fmt.Sprintf("listen=%s agent_grpc=%s db.driver=%s db.dsn=%s log.level=%s ch.ttl=%dd",
		c.Listen, c.AgentGRPC, c.DBDriver, maskDSN(c.DBDsn), c.LogLevel, c.CHTTLDays)
}

// maskDSN 仅保留协议与主机，隐藏凭据。
func maskDSN(s string) string {
	if i := strings.Index(s, "@"); i >= 0 {
		host := s[i+1:]
		prefix := s[:strings.LastIndex(s[:i], "/")+1]
		return prefix + "***:***@" + host
	}
	return "***"
}
