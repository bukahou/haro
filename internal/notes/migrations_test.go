package notes

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/sqliteschema"
)

// schemaVersion 读出库现在是第几版（goose 记的）。
func schemaVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var version int64
	if err := db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("读版本：%v", err)
	}
	return version
}

// 改用迁移之前建的记事库（同样的表，没有版本表）：打开后原来的记录都在（删掉的也在）、编号接着排，
// 升到最新一版；再打开不重复迁移。
func TestOpenSQLiteKeepsPreMigrationDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "notes.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `CREATE TABLE notes (id INTEGER PRIMARY KEY AUTOINCREMENT, content TEXT NOT NULL,
			created_at TEXT NOT NULL, deleted_at TEXT);
		CREATE INDEX notes_created_at ON notes (created_at);
		INSERT INTO notes (content, created_at) VALUES ('车停在 C3', '2026-11-01T10:00:00+09:00');
		INSERT INTO notes (content, created_at, deleted_at) VALUES ('旧的一条', '2026-11-02T10:00:00+09:00', '2026-11-03T10:00:00+09:00');`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	store, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("打开迁移之前建的库：%v", err)
	}
	if list, total, err := store.List(ctx, 10); err != nil || total != 1 || list[0].ID != 1 || list[0].Content != "车停在 C3" {
		t.Errorf("原来的记录应都在：%+v total=%d %v", list, total, err)
	}
	if deleted, err := store.Deleted(ctx, 10); err != nil || len(deleted) != 1 || deleted[0].ID != 2 {
		t.Errorf("删掉的也应在：%+v %v", deleted, err)
	}
	if saved, err := store.Save(ctx, "新的一条"); err != nil || saved.ID != 3 {
		t.Errorf("新记录的编号应接着排：%+v %v", saved, err)
	}
	store.Close()

	store, err = OpenSQLite(ctx, path) // 再打开：已经是最新一版，不重复
	if err != nil {
		t.Fatalf("再打开：%v", err)
	}
	defer store.Close()
	if version := schemaVersion(t, store.db); version != 2 {
		t.Errorf("应升到第 2 版：%d", version)
	}
	if _, total, err := store.List(ctx, 10); err != nil || total != 2 {
		t.Errorf("再打开后记录照旧：total=%d %v", total, err)
	}
}

// openFirstVersion 建一个停在第 1 版的记事库（时间存文本），放进 rows（id 顺序），关掉。
func openFirstVersion(t *testing.T, rows [][3]any) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "notes.db")
	db, err := sqliteschema.Open(ctx, path, migrationFiles, sqliteschema.Options{}) // 只有 SQL 写的第 1 版
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, row := range rows {
		if _, err := db.ExecContext(ctx, `INSERT INTO notes (content, created_at, deleted_at) VALUES (?, ?, ?)`, row[0], row[1], row[2]); err != nil {
			t.Fatal(err)
		}
	}
	if version := schemaVersion(t, db); version != 1 {
		t.Fatalf("准备的库应停在第 1 版：%d", version)
	}
	return path
}

// 迁移 2：时间从带时区的文本改存 Unix 秒。每一条都在、编号不变、时刻不变（换算成 UTC）；
// 先后按真实的时刻排（按文本排时不同时区的会排错）；删掉的照样是删掉的；新记录的编号接着排。
func TestUnixTimeMigrationKeepsEveryNote(t *testing.T) {
	ctx := context.Background()
	path := openFirstVersion(t, [][3]any{
		{"东京时间写的", "2026-11-01T10:00:00+09:00", nil},                      // 01:00 UTC
		{"UTC 写的", "2026-11-01T02:00:00Z", nil},                           // 02:00 UTC：文本比上一条「小」，其实晚
		{"纽约时间写的", "2026-10-31T21:30:00-05:00", nil},                      // 02:30 UTC：最新
		{"删掉的", "2026-11-02T00:00:00+09:00", "2026-11-03T08:00:00+09:00"}, // 删除时刻 11-02 23:00 UTC
		{"后来删掉的", "2026-11-01T00:30:00Z", "2026-11-03T00:30:00Z"},         // 删除时刻比上一条晚 1.5 小时
	})
	store, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("迁移：%v", err)
	}
	defer store.Close()
	if version := schemaVersion(t, store.db); version != 2 {
		t.Errorf("应升到第 2 版：%d", version)
	}

	list, total, err := store.List(ctx, 10)
	if err != nil || total != 3 {
		t.Fatalf("没删的三条都应在：total=%d %v", total, err)
	}
	var order []int64
	for _, note := range list {
		order = append(order, note.ID)
	}
	if want := []int64{3, 2, 1}; !slices.Equal(order, want) {
		t.Errorf("应按真实时刻排（新的在前）：%v，应为 %v", order, want)
	}
	wantCreated := map[int64]time.Time{
		1: time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC),
		2: time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC),
		3: time.Date(2026, 11, 1, 2, 30, 0, 0, time.UTC),
	}
	for _, note := range list {
		if !note.CreatedAt.Equal(wantCreated[note.ID]) || note.CreatedAt.Location() != time.UTC {
			t.Errorf("记录 %d 的时刻：%v，应为 %v（UTC）", note.ID, note.CreatedAt, wantCreated[note.ID])
		}
	}
	deleted, err := store.Deleted(ctx, 10)
	if err != nil || len(deleted) != 2 || deleted[0].ID != 5 || deleted[1].ID != 4 {
		t.Fatalf("删掉的两条都应在，最近删的在前：%+v %v", deleted, err)
	}
	if !deleted[1].DeletedAt.Equal(time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)) ||
		!deleted[1].CreatedAt.Equal(time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)) || deleted[1].Content != "删掉的" {
		t.Errorf("删掉的那条：%+v", deleted[1])
	}
	var kinds []string
	rows, err := store.db.QueryContext(ctx, `SELECT DISTINCT typeof(created_at) FROM notes`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
	}
	rows.Close()
	if !slices.Equal(kinds, []string{"integer"}) {
		t.Errorf("时间应存成整数（Unix 秒）：%v", kinds)
	}
	if saved, err := store.Save(ctx, "迁移后记的"); err != nil || saved.ID != 6 {
		t.Errorf("新记录的编号应接着排：%+v %v", saved, err)
	}
	if restored, err := store.Restore(ctx, 4); err != nil || !restored.DeletedAt.IsZero() {
		t.Errorf("迁移后照样能恢复：%+v %v", restored, err)
	}
}

// 最后几条被彻底删过（编号用过、行不在了）：迁移后新记录也不重用那些编号。
func TestUnixTimeMigrationKeepsUsedIDs(t *testing.T) {
	ctx := context.Background()
	path := openFirstVersion(t, [][3]any{{"留着的", "2026-11-01T10:00:00+09:00", nil}, {"会被彻底删掉的", "2026-11-01T11:00:00+09:00", nil}})
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM notes WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	store, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if saved, err := store.Save(ctx, "迁移后记的"); err != nil || saved.ID != 3 {
		t.Errorf("用过的编号 2 不应重用：%+v %v", saved, err)
	}
}

// 有一条的时间转换不了：整个迁移失败（服务端起不来、说明是哪一条），库保持原样 —— 不写成 0、不丢记录。
func TestUnixTimeMigrationFailsOnUnreadableTime(t *testing.T) {
	for _, c := range []struct {
		name string
		rows [][3]any
	}{
		{"记下的时间坏了", [][3]any{{"好的", "2026-11-01T10:00:00+09:00", nil}, {"坏的", "昨天下午", nil}}},
		{"删除时间坏了", [][3]any{{"好的", "2026-11-01T10:00:00+09:00", nil}, {"坏的", "2026-11-02T10:00:00+09:00", "2026-13-45"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			path := openFirstVersion(t, c.rows)
			if store, err := OpenSQLite(ctx, path); err == nil {
				store.Close()
				t.Fatal("有转换不了的时间应迁移失败")
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if version := schemaVersion(t, raw); version != 1 {
				t.Errorf("迁移失败后应还停在第 1 版：%d", version)
			}
			var created string
			var deleted sql.NullString
			if err := raw.QueryRow(`SELECT created_at, deleted_at FROM notes WHERE id = 2`).Scan(&created, &deleted); err != nil {
				t.Fatalf("原来的记录应原样在：%v", err)
			}
			if created != c.rows[1][1] || (c.rows[1][2] != nil && deleted.String != c.rows[1][2]) {
				t.Errorf("原来的时间文本应原样保留：%q %q", created, deleted.String)
			}
		})
	}
}
