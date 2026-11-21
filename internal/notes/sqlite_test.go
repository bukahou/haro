package notes

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "data", "notes.db")) // 目录不存在时自动创建
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// 列出：新的在前，最多 limit 条，并告诉一共有几条（没全给时调用方要说明）。
func TestListNewestFirstWithTotal(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	var saved []Note
	for _, content := range []string{"第一条", "第二条", "第三条"} {
		note, err := store.Save(ctx, content)
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, note)
	}
	list, total, err := store.List(ctx, 2)
	if err != nil || total != 3 || len(list) != 2 || list[0].ID != saved[2].ID || list[1].ID != saved[1].ID {
		t.Fatalf("应给最近的 2 条（新的在前）、一共 3 条：%+v total=%d %v", list, total, err)
	}
	if _, err := store.Save(ctx, "  "); err == nil {
		t.Error("空内容应被拒绝")
	}
}

// 删除是软删除：不再列出、数据还在、能在「最近删掉的」里看到并恢复；重复删、恢复没删的都报 ErrNotFound。
func TestSoftDeleteAndRestore(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first, _ := store.Save(ctx, "第一条")
	second, _ := store.Save(ctx, "第二条")

	deleted, err := store.Delete(ctx, first.ID)
	if err != nil || deleted.Content != "第一条" || deleted.DeletedAt.IsZero() {
		t.Fatalf("删除返回 %+v %v", deleted, err)
	}
	if list, total, _ := store.List(ctx, 10); total != 1 || len(list) != 1 || list[0].ID != second.ID {
		t.Errorf("删除后不应再列出：%+v total=%d", list, total)
	}
	if _, err := store.Delete(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应返回 ErrNotFound，得到 %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&count); err != nil || count != 2 {
		t.Errorf("软删除后表里应还有 2 行，实际 %d（%v）", count, err)
	}
	if gone, err := store.Deleted(ctx, 10); err != nil || len(gone) != 1 || gone[0].ID != first.ID {
		t.Errorf("最近删掉的应有这一条：%+v %v", gone, err)
	}

	restored, err := store.Restore(ctx, first.ID)
	if err != nil || restored.ID != first.ID || !restored.DeletedAt.IsZero() {
		t.Fatalf("恢复返回 %+v %v", restored, err)
	}
	if _, total, _ := store.List(ctx, 10); total != 2 {
		t.Errorf("恢复后应重新列出：一共 %d 条", total)
	}
	if _, err := store.Restore(ctx, second.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("恢复没删的应返回 ErrNotFound，得到 %v", err)
	}
	if _, err := store.Delete(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除不存在的编号应返回 ErrNotFound，得到 %v", err)
	}
}

func TestOpenNeedsLocation(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo.Close()
	if _, err := Open(ctx, ""); err == nil {
		t.Error("没给存储位置应报错")
	}
}

// 删、恢复交回改完的那一条；没有这样的一条才是 ErrNotFound；库出错时交回出错的原因，不说成「没有这一条」
// （以前改完读回时出错也报 ErrNotFound，主人会以为没删）。
func TestMarkTellsNotFoundFromErrors(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	note, err := store.Save(ctx, "车停在C3的7号位。")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Delete(ctx, note.ID)
	if err != nil || deleted.ID != note.ID || deleted.Content != note.Content || deleted.DeletedAt.IsZero() {
		t.Fatalf("删除交回删掉的那一条：%+v %v", deleted, err)
	}
	if _, err := store.Delete(ctx, note.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("已经删过：ErrNotFound：%v", err)
	}
	if restored, err := store.Restore(ctx, note.ID); err != nil || !restored.DeletedAt.IsZero() {
		t.Errorf("恢复交回恢复的那一条：%+v %v", restored, err)
	}
	if _, err := store.Restore(ctx, note.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("没删过的恢复不了：ErrNotFound：%v", err)
	}
	store.Close()
	if _, err := store.Delete(ctx, note.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("库出错：交回出错的原因，不是「没有这一条」：%v", err)
	}
}
