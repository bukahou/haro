package spending

import (
	"context"
	"database/sql"
	"embed"

	"github.com/pressly/goose/v3"
)

// 账本的表结构由迁移文件管理（sqliteschema）；迁移 2 要先查有没有那两列，用 Go 写。
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// ledgerMigrations 是账本的 SQL 迁移之外、用 Go 写的那几步（交给 sqliteschema.Open）。
var ledgerMigrations = []goose.ProviderOption{
	goose.WithGoMigrations(goose.NewGoMigration(2, &goose.GoFunc{RunTx: addCacheColumns}, nil)),
}

// addCacheColumns（迁移 2）：给加缓存记账之前建的账本补上缓存读写两列（旧记录按 0）。
// 用迁移 1 新建的账本已经有这两列，这一步什么都不做——SQLite 没有「列不存在才加」的写法，只能先查。
func addCacheColumns(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info('charges')`)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, column := range []struct{ name, add string }{
		{"cache_read_tokens", `ALTER TABLE charges ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cache_write_tokens", `ALTER TABLE charges ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0`},
	} {
		if existing[column.name] {
			continue
		}
		if _, err := tx.ExecContext(ctx, column.add); err != nil {
			return err
		}
	}
	return nil
}
