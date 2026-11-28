package sqliteschema

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // 纯 Go 的 SQLite 驱动：编译进程序，不用装任何软件、不用起数据库服务
)

// Options 是打开一个库时各库不同的地方。
type Options struct {
	// Private：库里是家里人说过的话：目录 0700、库文件 0600（SQLite 自己建的文件是 0644；WAL 等附属文件沿用库文件的权限）。
	// 不设时目录按 0755 建
	Private bool
	// ForeignKeys：检查外键约束（SQLite 默认不检查）
	ForeignKeys bool
	// Migrate 是给 goose 的额外设置（例如用 Go 写的迁移步骤），见 Migrate
	Migrate []goose.ProviderOption
}

// BusyTimeout 是文件库里另一个连接拿着写锁时最多等多久再报忙（SQLite 的 busy_timeout）。
// 调用方等一次写库的期限照它定：再等下去就是库出了问题。
const BusyTimeout = 5 * time.Second

// 文件库统一的设置：等写锁最多 BusyTimeout；日志用 WAL（读写互不阻塞）。内存库用不着这两项。
var filePragmas = []string{fmt.Sprintf("busy_timeout(%d)", BusyTimeout.Milliseconds()), "journal_mode(WAL)"}

// BackupPath 是 path 这个库最近一次迁移之前的备份（见 backupBeforeMigrate）。
func BackupPath(path string) string { return path + ".before-migrate" }

// Open 打开（没有就建）path 上的 SQLite 库，并把表结构升到最新（migrations 是这个库的迁移文件，见 Migrate）。
// 目录没有就建；只开一个连接：SQLite 同时只有一个写者，内存库每个连接各是一份库。path 为空 = 只在内存里（进程退出就没了）。
func Open(ctx context.Context, path string, migrations fs.FS, options Options) (*sql.DB, error) {
	location, err := prepareLocation(path, options)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", location)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if path != "" {
		if err := backupBeforeMigrate(ctx, db, migrations, BackupPath(path), options.Migrate...); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := Migrate(ctx, db, migrations, options.Migrate...); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级表结构: %w", err)
	}
	return db, nil
}

// prepareLocation 建好目录（和要求只有自己能读的库文件），返回给驱动的 SQLite URI：
// 文件名、设置都按 URI 的规矩转义（文件名里有空格、问号、# 也打得开），驱动执行其中的 _pragma。
func prepareLocation(path string, options Options) (string, error) {
	var pragmas []string
	if path != "" {
		pragmas = append(pragmas, filePragmas...)
	}
	if options.ForeignKeys {
		pragmas = append(pragmas, "foreign_keys(1)")
	}
	query := url.Values{"_pragma": pragmas}.Encode()
	if path == "" {
		return (&url.URL{Scheme: "file", Opaque: ":memory:", RawQuery: query}).String(), nil
	}
	// URI 里的文件名要是绝对路径：相对路径会被当成主机名
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("库文件路径 %s: %w", path, err)
	}
	directoryMode := os.FileMode(0o755)
	if options.Private {
		directoryMode = 0o700
	}
	if err := os.MkdirAll(filepath.Dir(absolute), directoryMode); err != nil {
		return "", fmt.Errorf("创建库所在的目录: %w", err)
	}
	if options.Private { // 先以 0600 建文件，SQLite 再打开它（已有的文件不改权限）
		file, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return "", fmt.Errorf("建库文件: %w", err)
		}
		file.Close()
	}
	return (&url.URL{Scheme: "file", Path: absolute, RawQuery: query}).String(), nil
}
