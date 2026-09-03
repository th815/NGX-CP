package config

import (
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if c.Listen != ":8080" {
		t.Errorf("默认 listen 错误: %q", c.Listen)
	}
	if c.DBDriver != "sqlite" {
		t.Errorf("默认 db_driver 错误: %q", c.DBDriver)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("默认配置校验失败: %v", err)
	}
}

func TestEnvOverride(t *testing.T) {
	// 扁平 key 对应的环境变量即文档约定的 NGXCP_DB_DRIVER / NGXCP_LOG_LEVEL
	t.Setenv("NGXCP_LOG_LEVEL", "debug")
	t.Setenv("NGXCP_DB_DRIVER", "postgres")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if c.LogLevel != "debug" {
		t.Errorf("环境变量覆盖失败: log_level=%q", c.LogLevel)
	}
	if c.DBDriver != "postgres" {
		t.Errorf("环境变量覆盖失败: db_driver=%q", c.DBDriver)
	}
}

func TestValidateInvalidDriver(t *testing.T) {
	t.Setenv("NGXCP_DB_DRIVER", "oracle")
	c, _ := Load("")
	if err := c.Validate(); err == nil {
		t.Error("非法 db_driver 未报错")
	}
}

func TestLoadExampleFile(t *testing.T) {
	// 测试从包目录运行，样例位于仓库根 configs/
	c, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("加载样例失败: %v", err)
	}
	if c.DBDriver != "sqlite" || c.Listen != ":8080" {
		t.Errorf("样例解析异常: %+v", c)
	}
	if c.String() == "" {
		t.Error("String() 返回空")
	}
}
