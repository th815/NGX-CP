// Package repo 封装 ent 客户端的创建与数据库迁移，屏蔽 SQLite / PostgreSQL 双路差异。
//
// 关键约束（见 docs/tasks/M0-foundation.md T006）：
//   - SQLite 必须用纯 Go 驱动 modernc.org/sqlite（禁止 CGO，保证交叉编译与静态链接）
//   - PostgreSQL 用 pgx stdlib 驱动，经 ent 的 sql.OpenDB 接入
//   - 每个数据库相关任务都要验证 SQLite + PG 双路径
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/th/ngxcp/ent"

	// 纯 Go SQLite 驱动（注册名 "sqlite"），无 CGO
	_ "modernc.org/sqlite"
	// pgx stdlib 驱动（注册名 "pgx"），供 ent 接入 PostgreSQL
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open 根据 driver 打开一个 ent 客户端。
// driver ∈ {"sqlite","postgres"}；dsn 为对应数据源名。
// SQLite 的 dsn 建议带 ?cache=shared&_fk=1（开启外键），WAL 在此处统一开启。
func Open(driver, dsn string) (*ent.Client, error) {
	switch driver {
	case "sqlite":
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("open sqlite: %w", err)
		}
		// 开启 WAL 与外键（连接级；dsn 已含 _fk=1 时同样生效）
		if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite pragma: %w", err)
		}
		drv := entsql.OpenDB(dialect.SQLite, db)
		return ent.NewClient(ent.Driver(drv)), nil
	case "postgres":
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
		drv := entsql.OpenDB(dialect.Postgres, db)
		return ent.NewClient(ent.Driver(drv)), nil
	default:
		return nil, fmt.Errorf("unsupported db driver %q (期望 sqlite|postgres)", driver)
	}
}

// Migrate 在数据库中创建/更新全部 ent schema（幂等）。
// 接受可选的日志回调，便于迁移过程可见。
func Migrate(ctx context.Context, client *ent.Client) error {
	return client.Schema.Create(ctx)
}
