// Command ngxcp-migrate 应用 ent schema 到目标数据库（开发态迁移）。
// 由 `make migrate-dev` 调用，读取 NGXCP_DB_DRIVER / NGXCP_DB_DSN 环境变量。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/th/ngxcp/internal/config"
	"github.com/th/ngxcp/internal/repo"
)

func main() {
	var configPath = flag.String("config", "", "path to config file (yaml)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "config invalid: %v\n", err)
		os.Exit(1)
	}

	client, err := repo.Open(cfg.DBDriver, cfg.DBDsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db error: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	if err := repo.Migrate(context.Background(), client); err != nil {
		fmt.Fprintf(os.Stderr, "migrate error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("migrate ok: driver=%s dsn=%s\n", cfg.DBDriver, cfg.DBDsn)
}
