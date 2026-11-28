package conversationstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// 线程：建、列出（新的在前、带最近一句）、起标题、软删除 / 恢复；不是这个人的找不到；往前翻消息按编号分页、旧的在前。
func TestThreadsLifecycle(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	now := time.Unix(1_700_000_000, 0)
	if _, err := store.CreateThread(ctx, "主人", "t1", false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateThread(ctx, "主人", "t2", true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	record := brain.ConversationRecord{Person: "主人", Thread: "t1", TurnCount: 3, UpdatedAt: now,
		History: []brain.ConversationTurn{numberedTurn(1, "一", "答一"), numberedTurn(2, "二", "答二"), numberedTurn(3, "三", "答三")}}
	for i := range record.History {
		record.History[i].At = now.Add(time.Duration(10+i) * time.Minute)
	}
	if err := store.SaveConversation(record); err != nil {
		t.Fatal(err)
	}
	threads, err := store.Threads(ctx, "主人", false)
	if err != nil || len(threads) != 2 || threads[0].ID != "t1" || threads[0].LastText != "答三" || !threads[1].Incognito {
		t.Fatalf("列表：%+v %v", threads, err)
	}
	if err := store.SetThreadTitle(ctx, "主人", "t1", "猫的事"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ThreadOf(ctx, "别人", "t1"); !errors.Is(err, ErrThreadNotFound) {
		t.Errorf("不是这个人的：%v", err)
	}
	if err := store.DeleteThread(ctx, "主人", "t1", now); err != nil {
		t.Fatal(err)
	}
	if visible, _ := store.Threads(ctx, "主人", false); len(visible) != 1 {
		t.Errorf("删了的不列：%+v", visible)
	}
	if err := store.RestoreThread(ctx, "主人", "t1"); err != nil {
		t.Fatal(err)
	}
	if thread, err := store.ThreadOf(ctx, "主人", "t1"); err != nil || thread.Title != "猫的事" || thread.DeletedAt != nil {
		t.Errorf("恢复后：%+v %v", thread, err)
	}
	page, err := store.ThreadTurns(ctx, "主人", "t1", 0, 2)
	if err != nil || len(page) != 2 || page[0].Seq != 2 || page[1].Seq != 3 {
		t.Fatalf("最新的一页：%+v %v", page, err)
	}
	if older, _ := store.ThreadTurns(ctx, "主人", "t1", page[0].Seq, 2); len(older) != 1 || older[0].Seq != 1 {
		t.Errorf("再往前：%+v", older)
	}
}

// 无痕线程的轮次不算向量（长期记忆按意思找不到）。
func TestIncognitoTurnsAreNotIndexed(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	if _, err := store.CreateThread(ctx, "主人", "secret", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, record := range []brain.ConversationRecord{
		{Person: "主人", Thread: "secret", TurnCount: 1, UpdatedAt: time.Now(), History: []brain.ConversationTurn{numberedTurn(1, "无痕", "无痕答")}},
		{Person: "主人", TurnCount: 2, UpdatedAt: time.Now(), History: []brain.ConversationTurn{numberedTurn(2, "语音", "语音答")}},
	} {
		if err := store.SaveConversation(record); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := store.UnindexedTurns(ctx, "e5", 10)
	if err != nil || len(pending) != 1 || pending[0].Seq != 2 {
		t.Errorf("只算语音那轮：%+v %v", pending, err)
	}
}
