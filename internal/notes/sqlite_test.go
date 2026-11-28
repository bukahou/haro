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
		note, err := store.Save(ctx, NewNote{Content: content, Source: SourceOwner})
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, note)
	}
	list, total, err := store.List(ctx, "", 2)
	if err != nil || total != 3 || len(list) != 2 || list[0].ID != saved[2].ID || list[1].ID != saved[1].ID {
		t.Fatalf("应给最近的 2 条（新的在前）、一共 3 条：%+v total=%d %v", list, total, err)
	}
	if _, err := store.Save(ctx, NewNote{Content: "  ", Source: SourceOwner}); err == nil {
		t.Error("空内容应被拒绝")
	}
}

// 删除是软删除：不再列出、数据还在、能在「最近删掉的」里看到并恢复；重复删、恢复没删的都报 ErrNotFound。
func TestSoftDeleteAndRestore(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first, _ := store.Save(ctx, NewNote{Content: "第一条", Source: SourceOwner})
	second, _ := store.Save(ctx, NewNote{Content: "第二条", Source: SourceOwner})

	deleted, err := store.Delete(ctx, "", first.ID)
	if err != nil || deleted.Content != "第一条" || deleted.DeletedAt.IsZero() {
		t.Fatalf("删除返回 %+v %v", deleted, err)
	}
	if list, total, _ := store.List(ctx, "", 10); total != 1 || len(list) != 1 || list[0].ID != second.ID {
		t.Errorf("删除后不应再列出：%+v total=%d", list, total)
	}
	if _, err := store.Delete(ctx, "", first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应返回 ErrNotFound，得到 %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&count); err != nil || count != 2 {
		t.Errorf("软删除后表里应还有 2 行，实际 %d（%v）", count, err)
	}
	if gone, err := store.Deleted(ctx, "", 10); err != nil || len(gone) != 1 || gone[0].ID != first.ID {
		t.Errorf("最近删掉的应有这一条：%+v %v", gone, err)
	}

	restored, err := store.Restore(ctx, "", first.ID)
	if err != nil || restored.ID != first.ID || !restored.DeletedAt.IsZero() {
		t.Fatalf("恢复返回 %+v %v", restored, err)
	}
	if _, total, _ := store.List(ctx, "", 10); total != 2 {
		t.Errorf("恢复后应重新列出：一共 %d 条", total)
	}
	if _, err := store.Restore(ctx, "", second.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("恢复没删的应返回 ErrNotFound，得到 %v", err)
	}
	if _, err := store.Delete(ctx, "", 999); !errors.Is(err, ErrNotFound) {
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
	note, err := store.Save(ctx, NewNote{Content: "车停在C3的7号位。", Source: SourceOwner})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Delete(ctx, "", note.ID)
	if err != nil || deleted.ID != note.ID || deleted.Content != note.Content || deleted.DeletedAt.IsZero() {
		t.Fatalf("删除交回删掉的那一条：%+v %v", deleted, err)
	}
	if _, err := store.Delete(ctx, "", note.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("已经删过：ErrNotFound：%v", err)
	}
	if restored, err := store.Restore(ctx, "", note.ID); err != nil || !restored.DeletedAt.IsZero() {
		t.Errorf("恢复交回恢复的那一条：%+v %v", restored, err)
	}
	if _, err := store.Restore(ctx, "", note.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("没删过的恢复不了：ErrNotFound：%v", err)
	}
	store.Close()
	if _, err := store.Delete(ctx, "", note.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("库出错：交回出错的原因，不是「没有这一条」：%v", err)
	}
}

// 按人：只看得到、只改得了自己的和全家共用的；常驻的单独取；按编号取（删掉的也给，看不到的不给）。
func TestNotesArePerPerson(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	shared, _ := store.Save(ctx, NewNote{Content: "钥匙在玄关的抽屉里", Source: SourceOwner})
	mine, _ := store.Save(ctx, NewNote{Person: "主人", Content: "不吃香菜", Source: SourceModel, Resident: true})
	theirs, _ := store.Save(ctx, NewNote{Person: "老大", Content: "周五要交作业", Source: SourceOwner})
	if _, err := store.Save(ctx, NewNote{Person: "主人", Content: "随便", Source: "guess"}); err == nil {
		t.Error("不认得的来源应拒绝")
	}

	list, total, err := store.List(ctx, "主人", 10)
	if err != nil || total != 2 || len(list) != 2 || list[0].ID != mine.ID || list[1].ID != shared.ID {
		t.Fatalf("主人看得到自己的和全家共用的：%+v %d %v", list, total, err)
	}
	if mine.Source != SourceModel || !mine.Resident || list[0].Source != SourceModel || !list[0].Resident {
		t.Errorf("来源、常驻应存下：%+v", list[0])
	}
	if _, err := store.Delete(ctx, "主人", theirs.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("不能删别人的：%v", err)
	}
	if resident, err := store.Resident(ctx, "主人", 10); err != nil || len(resident) != 1 || resident[0].ID != mine.ID {
		t.Errorf("常驻的：%+v %v", resident, err)
	}
	if resident, _ := store.Resident(ctx, "老大", 10); len(resident) != 0 {
		t.Errorf("别人的常驻看不到：%+v", resident)
	}
	if _, err := store.Delete(ctx, "主人", shared.ID); err != nil {
		t.Fatalf("全家共用的谁都能删：%v", err)
	}
	got, err := store.Get(ctx, "主人", []int64{theirs.ID, shared.ID, mine.ID})
	if err != nil || len(got) != 2 || got[0].ID != shared.ID || got[0].DeletedAt.IsZero() || got[1].ID != mine.ID {
		t.Errorf("按编号取：按顺序、删掉的也给、别人的不给：%+v %v", got, err)
	}
}

// 向量：还没有这个模型的向量的才要算；删掉的不出现在向量里；换了模型就要重算。
func TestNoteVectors(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first, _ := store.Save(ctx, NewNote{Person: "主人", Content: "不吃香菜", Source: SourceModel})
	second, _ := store.Save(ctx, NewNote{Content: "钥匙在抽屉里", Source: SourceOwner})
	pending, err := store.Unindexed(ctx, "e5", 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("都还没有向量：%+v %v", pending, err)
	}
	if err := store.SaveVector(ctx, first.ID, "e5", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if pending, _ := store.Unindexed(ctx, "e5", 10); len(pending) != 1 || pending[0].ID != second.ID {
		t.Errorf("算过的不再要：%+v", pending)
	}
	if pending, _ := store.Unindexed(ctx, "bge", 10); len(pending) != 2 {
		t.Errorf("换了模型都要重算：%+v", pending)
	}
	_ = store.SaveVector(ctx, second.ID, "e5", []float32{0, 1})
	if _, err := store.Delete(ctx, "", second.ID); err != nil {
		t.Fatal(err)
	}
	vectors, err := store.Vectors(ctx, "e5")
	if err != nil || len(vectors) != 1 || vectors[0].ID != first.ID || vectors[0].Person != "主人" || vectors[0].Vector[0] != 1 {
		t.Errorf("删掉的不出现：%+v %v", vectors, err)
	}
	if vector, err := store.Vector(ctx, second.ID, "e5"); err != nil || len(vector) != 2 || vector[1] != 1 {
		t.Errorf("删掉的那条的向量照样取得到（恢复时用）：%v %v", vector, err)
	}
	if vector, err := store.Vector(ctx, second.ID, "bge"); err != nil || vector != nil {
		t.Errorf("没有这个模型的向量时为 nil：%v %v", vector, err)
	}
}

// 彻底删账号：删掉这个人记的（含向量），别人的、全家共用的不动。
func TestErasePerson(t *testing.T) {
	store, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, person := range []string{"走的人", "别人", ""} {
		note, err := store.Save(ctx, NewNote{Person: person, Content: "猫粮在柜子里", Source: SourceOwner})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveVector(ctx, note.ID, "m", []float32{1, 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ErasePerson(ctx, "走的人"); err != nil {
		t.Fatal(err)
	}
	for person, want := range map[string]int{"走的人": 1, "别人": 2} { // 列出时带着全家共用的那条
		if _, total, err := store.List(ctx, person, 10); err != nil || total != want {
			t.Errorf("%s：%d %v", person, total, err)
		}
	}
	if vectors, _ := store.Vectors(ctx, "m"); len(vectors) != 2 {
		t.Errorf("向量：%d", len(vectors))
	}
}
