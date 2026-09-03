// Command ngxcp-server 是 NGX-CP 控制面（control plane）入口。
// M0 阶段仅做骨架：加载配置、初始化日志、报告就绪；业务逻辑在 M1+ 实现。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/th/ngxcp/internal/config"
	"github.com/th/ngxcp/internal/pkg/logging"
	"github.com/th/ngxcp/internal/pkg/version"
	"github.com/th/ngxcp/internal/server"
)

func main() {
	var (
		configPath  = flag.String("config", "", "path to config file (yaml)")
		showVersion = flag.Bool("version", false, "print version and exit")
		checkConfig = flag.Bool("check-config", false, "load & validate config then exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "config invalid: %v\n", err)
		os.Exit(1)
	}
	if *checkConfig {
		fmt.Println(cfg.String())
		return
	}

	if err := logging.Init(cfg.LogLevel, cfg.LogPretty); err != nil {
		fmt.Fprintf(os.Stderr, "logging init error: %v\n", err)
		os.Exit(1)
	}

	if err := server.Run(cfg); err != nil {
		logging.Ctx(nil).Error().Err(err).Msg("server exited")
		os.Exit(1)
	}
}
