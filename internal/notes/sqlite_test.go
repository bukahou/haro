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

func TestSearchMatchesRephrasedQuery(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for _, content := range []string{"车停在C3的7号位。", "周三下午去取干洗的衣服。", "钥匙放在玄关的抽屉里。"} {
		if _, err := store.Save(ctx, content); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]string{
		"车停在哪":  "车停在C3的7号位。", // 实测里模型给的查询词
		"干洗":    "周三下午去取干洗的衣服。",
		"钥匙在哪里": "钥匙放在玄关的抽屉里。",
		"C3":    "车停在C3的7号位。",
	}
	for query, want := range cases {
		found, err := store.Search(ctx, query, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) == 0 || found[0].Content != want {
			t.Errorf("Search(%q) = %+v，第一条应为 %q", query, found, want)
		}
	}
	if found, _ := store.Search(ctx, "天气怎么样", 3); len(found) != 0 {
		t.Errorf("不相关的查询应返回空，得到 %+v", found)
	}
}

func TestRecentAndSoftDelete(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first, _ := store.Save(ctx, "第一条")
	second, _ := store.Save(ctx, "第二条")
	recent, err := store.Recent(ctx, 5)
	if err != nil || len(recent) != 2 || recent[0].ID != second.ID {
		t.Fatalf("最近的应在前：%+v %v", recent, err)
	}
	deleted, err := store.Delete(ctx, first.ID)
	if err != nil || deleted.Content != "第一条" {
		t.Fatalf("删除返回 %+v %v", deleted, err)
	}
	if recent, _ := store.Recent(ctx, 5); len(recent) != 1 {
		t.Errorf("删除后不应再列出：%+v", recent)
	}
	if _, err := store.Delete(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应返回 ErrNotFound，得到 %v", err)
	}
	// 软删除：数据还在
	var count int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&count)
	if count != 2 {
		t.Errorf("软删除后表里应还有 2 行，实际 %d", count)
	}
	if _, err := store.Save(ctx, "  "); err == nil {
		t.Error("空内容应被拒绝")
	}
}

func TestOpenChoosesImplementation(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo.Close()
	for _, location := range []string{"postgres://u@h/db", "mysql://u@h/db", ""} {
		if _, err := Open(ctx, location); err == nil {
			t.Errorf("Open(%q) 现在应返回「暂不支持」或「没有指定」", location)
		}
	}
}

func TestRankByRelevanceKeepsNewestFirstOnTies(t *testing.T) {
	candidates := []Note{{ID: 2, Content: "钥匙在抽屉里"}, {ID: 1, Content: "钥匙在门口"}} // 按时间倒序
	ranked := RankByRelevance(candidates, "钥匙", 5)
	if len(ranked) != 2 || ranked[0].ID != 2 {
		t.Errorf("同分时新的应在前：%+v", ranked)
	}
}
