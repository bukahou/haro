// Package sqliteschema 管各个 SQLite 库的表结构：每个库的迁移文件放在自己包的 migrations/ 目录、编进程序，
// 由 goose 按版本只跑一次（版本记在库里的 goose_db_version 表）。
package sqliteschema

import (
	"context"
	"database/sql"
	"io/fs"

	"github.com/pressly/goose/v3"
)

// Migrate 把 db 的表结构升到最新。files 是用 //go:embed migrations/*.sql 编进程序的迁移文件；
// options 是给 goose 的额外设置（例如用 Go 写的迁移步骤）。
func Migrate(ctx context.Context, db *sql.DB, files fs.FS, options ...goose.ProviderOption) error {
	migrations, err := fs.Sub(files, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations,
		append([]goose.ProviderOption{goose.WithDisableGlobalRegistry(true)}, options...)...)
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}
