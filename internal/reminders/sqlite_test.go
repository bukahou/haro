package reminders_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/reminders"
)

func openStore(t *testing.T) *reminders.SQLiteStore {
	t.Helper()
	store, err := reminders.OpenSQLite(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

var base = time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)

func add(t *testing.T, store reminders.Store, person, text string, next time.Time) reminders.Reminder {
	t.Helper()
	r, err := store.Add(context.Background(), reminders.Reminder{Person: person, Text: text, StartsAt: next, NextAt: next})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 和记事本放在同一个库文件里：两边的表结构各升各的（版本记在不同的表），互不影响，重开也照常。
func TestSharesFileWithNotes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "notes.db")
	for range 2 { // 第二次：两边都已经是最新的
		noteStore, err := notes.OpenSQLite(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		reminderStore, err := reminders.OpenSQLite(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := noteStore.Save(ctx, "买牛奶"); err != nil {
			t.Fatal(err)
		}
		add(t, reminderStore, "主人", "该喝水了", base)
		_ = noteStore.Close()
		_ = reminderStore.Close()
	}
	reminderStore, _ := reminders.OpenSQLite(ctx, path)
	defer reminderStore.Close()
	if count, _ := reminderStore.CountActive(ctx, "主人"); count != 2 {
		t.Errorf("重开后应有 2 条提醒：%d", count)
	}
}

// 取消是软删除、能恢复；按人隔开；改不到（不是他的、状态不对）时返回 ErrNotFound。
func TestCancelRestoreAndOwnership(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	mine := add(t, store, "主人", "该喝水了", base.Add(time.Hour))
	add(t, store, "老大", "写作业", base.Add(2*time.Hour))

	if _, err := store.Cancel(ctx, "老大", mine.ID, base); !errors.Is(err, reminders.ErrNotFound) {
		t.Errorf("不能取消别人的提醒：%v", err)
	}
	cancelled, err := store.Cancel(ctx, "主人", mine.ID, base)
	if err != nil || cancelled.CancelledAt.IsZero() {
		t.Fatalf("取消：%v %+v", err, cancelled)
	}
	if _, err := store.Cancel(ctx, "主人", mine.ID, base); !errors.Is(err, reminders.ErrNotFound) {
		t.Errorf("已经取消的再取消：%v", err)
	}
	if active, _ := store.Active(ctx, "主人"); len(active) != 0 {
		t.Errorf("取消后不在在用的里面：%v", active)
	}
	if due, _ := store.Due(ctx, base.Add(24*time.Hour)); len(due) != 1 || due[0].Person != "老大" {
		t.Errorf("取消了的不会到点：%v", due)
	}
	if list, _ := store.Cancelled(ctx, "主人", 10); len(list) != 1 {
		t.Errorf("最近取消的应有这一条：%v", list)
	}
	restored, err := store.Restore(ctx, "主人", mine.ID, base.Add(3*time.Hour))
	if err != nil || !restored.CancelledAt.IsZero() || !restored.NextAt.Equal(base.Add(3*time.Hour)) {
		t.Fatalf("恢复：%v %+v", err, restored)
	}
	if _, err := store.Restore(ctx, "主人", mine.ID, base); !errors.Is(err, reminders.ErrNotFound) {
		t.Errorf("没取消的不能恢复：%v", err)
	}
}

// 到点、说过、错过：一次性的说过就不再到点；错过的记下来，告诉主人之后清掉。
func TestDueSpokenAndMissed(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	first := add(t, store, "主人", "该喝水了", base.Add(time.Minute))
	second := add(t, store, "主人", "吃药", base.Add(2*time.Minute))
	if next, ok, _ := store.NextDue(ctx); !ok || !next.Equal(first.NextAt) {
		t.Errorf("最早到点：%v %v", next, ok)
	}
	due, _ := store.Due(ctx, base.Add(90*time.Second))
	if len(due) != 1 || due[0].ID != first.ID {
		t.Fatalf("到点的只有第一条：%v", due)
	}
	if err := store.Spoken(ctx, first.ID, base.Add(90*time.Second), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Missed(ctx, second.ID, second.NextAt, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.NextDue(ctx); ok {
		t.Error("都不会再到点了")
	}
	missed, _ := store.Unannounced(ctx, "主人")
	if len(missed) != 1 || missed[0].ID != second.ID || !missed[0].MissedAt.Equal(second.NextAt) {
		t.Fatalf("错过的应记下：%v", missed)
	}
	if err := store.MissTold(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if missed, _ := store.Unannounced(ctx, "主人"); len(missed) != 0 {
		t.Errorf("告诉之后清掉：%v", missed)
	}
	spoken, _ := store.Get(ctx, "主人", first.ID)
	if spoken.SpokenAt.IsZero() || spoken.Active() {
		t.Errorf("说过的一次性提醒：%+v", spoken)
	}
}
