package notes

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// 改用迁移之前建的记事库（同样的表，没有版本表）：打开后原来的记录都在（删掉的也在）、编号接着排，
// 记成第 1 版；再打开不重复迁移。
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

	store, err = OpenSQLite(ctx, path) // 再打开：已经是第 1 版，不重复
	if err != nil {
		t.Fatalf("再打开：%v", err)
	}
	defer store.Close()
	var version int64
	if err := store.db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM goose_db_version`).Scan(&version); err != nil || version != 1 {
		t.Errorf("应记成第 1 版：%d %v", version, err)
	}
	if _, total, err := store.List(ctx, 10); err != nil || total != 2 {
		t.Errorf("再打开后记录照旧：total=%d %v", total, err)
	}
}
