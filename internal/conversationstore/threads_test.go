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

// 按文字找：主人说的、haro 答的都算（大小写不论、% 照字面），系统附注不算，删了的对话里的不算；按编号取回几轮带着在哪条对话。
func TestSearchTurns(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	if _, err := store.CreateThread(ctx, "主人", "gone", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	note := brain.ChatMessage{Role: brain.RoleUser, Text: "【现在的情况】猫粮快没了", Synthetic: true}
	voice := brain.ConversationRecord{Person: "主人", TurnCount: 2, UpdatedAt: time.Now(), History: []brain.ConversationTurn{
		numberedTurn(1, "Haro 帮我记一下猫粮", "好的，记下猫粮了"),
		{Seq: 2, At: time.Unix(1_700_000_002, 0), Messages: []brain.ChatMessage{note, {Role: brain.RoleAssistant, Text: "100% 确定"}}}}}
	gone := brain.ConversationRecord{Person: "主人", Thread: "gone", TurnCount: 3, UpdatedAt: time.Now(),
		History: []brain.ConversationTurn{numberedTurn(3, "猫粮买哪家", "删掉的对话")}}
	for _, record := range []brain.ConversationRecord{voice, gone} {
		if err := store.SaveConversation(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DeleteThread(ctx, "主人", "gone", time.Now()); err != nil {
		t.Fatal(err)
	}
	if found, err := store.SearchTurns(ctx, "主人", "猫粮", 10); err != nil || len(found) != 1 || found[0].Seq != 1 {
		t.Errorf("猫粮（附注里的、删了的对话里的不算）：%+v %v", found, err)
	}
	if found, _ := store.SearchTurns(ctx, "主人", "haro", 10); len(found) != 1 {
		t.Errorf("大小写不论：%+v", found)
	}
	if found, _ := store.SearchTurns(ctx, "主人", "100%", 10); len(found) != 1 || found[0].Seq != 2 {
		t.Errorf("%% 照字面：%+v", found)
	}
	if found, _ := store.SearchTurns(ctx, "主人", "%", 10); len(found) != 1 {
		t.Errorf("只有 %% 时只找含 %% 的：%+v", found)
	}
	if found, _ := store.SearchTurns(ctx, "别人", "猫粮", 10); len(found) != 0 {
		t.Errorf("别人的：%+v", found)
	}
	if found, err := store.TurnsBySeq(ctx, "主人", []int64{1, 3}); err != nil || len(found) != 1 || found[0].Thread != brain.VoiceThread {
		t.Errorf("按编号取回（删了的对话不给）：%+v %v", found, err)
	}
	if err := store.AddAttachment(ctx, Attachment{ID: "a1", Person: "主人", Thread: "gone", Width: 10, Height: 5, Bytes: 100,
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.AttachmentOf(ctx, "主人", "a1"); err != nil || got.Thread != "gone" || got.Width != 10 {
		t.Errorf("图片：%+v %v", got, err)
	}
	if _, err := store.AttachmentOf(ctx, "别人", "a1"); !errors.Is(err, ErrAttachmentNotFound) {
		t.Errorf("别人的图片：%v", err)
	}
}

// 彻底删账号：删掉这个人的各条对话、完整记录、背景记录池、附件的记录（交回附件编号），别人的不动。
func TestErasePerson(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	for _, person := range []string{"走的人", "别人"} {
		if err := store.SaveConversation(brain.ConversationRecord{Person: person, TurnCount: 1,
			History: []brain.ConversationTurn{numberedTurn(1, "问", "答")}, UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateThread(ctx, person, "t-"+person, false, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := store.AddAttachment(ctx, Attachment{ID: "img-" + person, Person: person, Thread: "t-" + person, Width: 1, Height: 1,
			Bytes: 1, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := store.ErasePerson(ctx, "走的人")
	if err != nil || len(ids) != 1 || ids[0] != "img-走的人" {
		t.Fatalf("删：%v %v", ids, err)
	}
	if _, ok, _ := store.LoadConversation("走的人", brain.VoiceThread); ok {
		t.Error("对话应删掉")
	}
	if threads, _ := store.Threads(ctx, "走的人", true); len(threads) != 0 {
		t.Errorf("线程：%+v", threads)
	}
	if last, _ := store.LastSeq("走的人"); last != 0 {
		t.Errorf("完整记录：%d", last)
	}
	if _, ok, _ := store.LoadConversation("别人", brain.VoiceThread); !ok {
		t.Error("别人的对话不动")
	}
	if _, err := store.AttachmentOf(ctx, "别人", "img-别人"); err != nil {
		t.Errorf("别人的附件：%v", err)
	}
}
