package research

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// 改用迁移之前建的任务库（同样的表，没有版本表）：打开后原来的任务都在、编号接着排，记成第 1 版；再打开不重复迁移。
func TestOpenTaskStoreKeepsPreMigrationDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "research.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `CREATE TABLE research_tasks (id INTEGER PRIMARY KEY AUTOINCREMENT, person TEXT NOT NULL,
			question TEXT NOT NULL, status TEXT NOT NULL, conclusion TEXT NOT NULL DEFAULT '', sources TEXT NOT NULL DEFAULT '',
			stop_reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0,
			reported INTEGER NOT NULL DEFAULT 0);
		CREATE INDEX research_tasks_person ON research_tasks (person, id);
		INSERT INTO research_tasks (person, question, status, conclusion, sources, created_at, finished_at, reported)
			VALUES ('owner', '旧的问题', 'done', '旧的结论', 'https://example.com/a', 1700000000, 1700000100, 1);`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	store, err := OpenTaskStore(ctx, path)
	if err != nil {
		t.Fatalf("打开迁移之前建的库：%v", err)
	}
	if task, err := store.Get(ctx, 1); err != nil || task.Question != "旧的问题" || task.Conclusion != "旧的结论" ||
		len(task.Sources) != 1 || !task.Reported {
		t.Errorf("原来的任务应都在：%+v %v", task, err)
	}
	if task, err := store.Create(ctx, "owner", "新的问题"); err != nil || task.ID != 2 {
		t.Errorf("新任务的编号应接着排：%+v %v", task, err)
	}
	store.Close()

	store, err = OpenTaskStore(ctx, path) // 再打开：已经是第 1 版，不重复
	if err != nil {
		t.Fatalf("再打开：%v", err)
	}
	defer store.Close()
	var version int64
	if err := store.(*sqliteTaskStore).db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM goose_db_version`).Scan(&version); err != nil || version != 1 {
		t.Errorf("应记成第 1 版：%d %v", version, err)
	}
	if task, err := store.Get(ctx, 2); err != nil || task.Question != "新的问题" {
		t.Errorf("再打开后任务照旧：%+v %v", task, err)
	}
}
