// Package sqliteschema 管各个 SQLite 库的打开方式（Open：同一套设置）和表结构：每个库的迁移文件放在自己包的
// migrations/ 目录、编进程序，由 goose 按版本只跑一次（版本记在库里的 goose_db_version 表）。
package sqliteschema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/pressly/goose/v3"
)

// Migrate 把 db 的表结构升到最新。files 是用 //go:embed migrations/*.sql 编进程序的迁移文件；
// options 是给 goose 的额外设置（例如用 Go 写的迁移步骤）。
func Migrate(ctx context.Context, db *sql.DB, files fs.FS, options ...goose.ProviderOption) error {
	provider, err := newProvider(db, files, options...)
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

func newProvider(db *sql.DB, files fs.FS, options ...goose.ProviderOption) (*goose.Provider, error) {
	migrations, err := fs.Sub(files, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectSQLite3, db, migrations,
		append([]goose.ProviderOption{goose.WithDisableGlobalRegistry(true)}, options...)...)
}

// backupBeforeMigrate 在有待跑的迁移时，先把库原样存一份到 backup（SQLite 的 VACUUM INTO：一致的快照，
// 库开着也能做）。迁移出了错（哪怕事务回滚了）、或迁移后发现数据不对，都能拿它还原。上一次的备份被这一次的替换：
// 留的总是最近一次迁移之前的样子。库还是空的（第一次建）时不备份。
func backupBeforeMigrate(ctx context.Context, db *sql.DB, files fs.FS, backup string, options ...goose.ProviderOption) error {
	provider, err := newProvider(db, files, options...)
	if err != nil {
		return err
	}
	pending, err := provider.HasPending(ctx)
	if err != nil || !pending {
		return err
	}
	current, err := provider.GetDBVersion(ctx)
	if err != nil || current == 0 {
		return err
	}
	if err := os.Remove(backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("删掉上一次迁移前的备份 %s: %w", backup, err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		return fmt.Errorf("迁移前备份到 %s: %w", backup, err)
	}
	return os.Chmod(backup, 0o600)
}
