package sqliteschema

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"
)

var testMigrations = fstest.MapFS{"migrations/00001_items.sql": {Data: []byte(`-- +goose Up
CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL);`)}}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var value string
	if err := db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return value
}

// 文件库：没有的目录自动建（文件名里有空格、问号、#、中文也打得开），统一的等锁时间与 WAL，只开一个连接，表结构升到最新；
// 再打开是同一个库。
func TestOpenFileDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "新 目录", "a b?c#d.db")
	db, err := Open(ctx, path, testMigrations, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("库文件应建在给的路径上：%v", err)
	}
	if got := pragma(t, db, "busy_timeout"); got != strconv.FormatInt(BusyTimeout.Milliseconds(), 10) {
		t.Errorf("等锁时间：%s", got)
	}
	if got := pragma(t, db, "journal_mode"); got != "wal" {
		t.Errorf("日志模式：%s", got)
	}
	if got := pragma(t, db, "foreign_keys"); got != "0" {
		t.Errorf("没要求时不检查外键：%s", got)
	}
	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("只开一个连接：%d", got)
	}
	if _, err := db.Exec(`INSERT INTO items (name) VALUES ('第一件')`); err != nil {
		t.Fatalf("表结构应已升到最新：%v", err)
	}
	db.Close()

	again, err := Open(ctx, path, testMigrations, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var count int
	if err := again.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&count); err != nil || count != 1 {
		t.Errorf("再打开应是同一个库：%d %v", count, err)
	}
}

// 存家里人说过的话的库：目录 0700、文件 0600；要求时检查外键。
func TestOpenPrivateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "talk.db")
	db, err := Open(context.Background(), path, testMigrations, Options{Private: true, ForeignKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("目录应是 0700：%v %v", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("库文件应是 0600：%v %v", info.Mode().Perm(), err)
	}
	if got := pragma(t, db, "foreign_keys"); got != "1" {
		t.Errorf("要求时应检查外键：%s", got)
	}
}

// 路径为空：只在内存里，一直是同一份库（只有一个连接）。
func TestOpenMemoryDatabase(t *testing.T) {
	db, err := Open(context.Background(), "", testMigrations, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for range 3 {
		if _, err := db.Exec(`INSERT INTO items (name) VALUES ('一件')`); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&count); err != nil || count != 3 {
		t.Errorf("内存库应一直是同一份：%d %v", count, err)
	}
	if got := pragma(t, db, "journal_mode"); got != "memory" {
		t.Errorf("内存库不用 WAL：%s", got)
	}
}

// 迁移失败：报错，不交出半升级的库。
func TestOpenFailsWhenMigrationFails(t *testing.T) {
	broken := fstest.MapFS{"migrations/00001_broken.sql": {Data: []byte("-- +goose Up\nCREATE TABLE (;")}}
	if db, err := Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), broken, Options{}); err == nil {
		db.Close()
		t.Error("迁移写错了应打开失败")
	}
}

// 有待跑的迁移时先备份：第一次建库不备份；加了新迁移再打开，备份里是迁移之前的样子（有原来的数据、没有新表）、只有自己能读。
func TestBackupBeforeMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := Open(context.Background(), path, testMigrations, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO items (name) VALUES ('原来的')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(BackupPath(path)); !os.IsNotExist(err) {
		t.Errorf("第一次建库不该有备份：%v", err)
	}
	more := fstest.MapFS{"migrations/00001_items.sql": testMigrations["migrations/00001_items.sql"],
		"migrations/00002_more.sql": {Data: []byte("-- +goose Up\nCREATE TABLE more (id INTEGER PRIMARY KEY);")}}
	db, err = Open(context.Background(), path, more, Options{})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	backup, err := sql.Open("sqlite", BackupPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var name string
	if err := backup.QueryRow(`SELECT name FROM items`).Scan(&name); err != nil || name != "原来的" {
		t.Errorf("备份里的数据：%q %v", name, err)
	}
	if err := backup.QueryRow(`SELECT name FROM sqlite_master WHERE name = 'more'`).Scan(&name); err == nil {
		t.Error("备份是迁移之前的：不该有新表")
	}
	if info, err := os.Stat(BackupPath(path)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("备份的权限：%v %v", info.Mode(), err)
	}
}
