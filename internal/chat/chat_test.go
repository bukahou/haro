package chat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/realtimeproto"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeSession 收到一句就发出 deltas，等 release 关了（或被停止）再结束。
type fakeSession struct {
	events  chan realtimeproto.ServerEvent
	release chan struct{}
	once    sync.Once
	closed  atomic.Bool
	said    string
}

func newFakeSession() *fakeSession {
	return &fakeSession{events: make(chan realtimeproto.ServerEvent, 8), release: make(chan struct{})}
}

func (f *fakeSession) SubmitText(input brain.TextInput) error {
	f.said = input.Text
	go func() {
		f.events <- realtimeproto.ResponseCreated{}
		f.events <- realtimeproto.ResponseOutputAudioTranscriptDelta{Delta: "好的"}
		<-f.release
		f.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	}()
	return nil
}
func (f *fakeSession) Events() <-chan realtimeproto.ServerEvent { return f.events }
func (f *fakeSession) Send(_ context.Context, event realtimeproto.ClientEvent) error {
	if _, ok := event.(realtimeproto.ResponseCancel); ok {
		f.once.Do(func() { close(f.release) })
	}
	return nil
}
func (f *fakeSession) Close() error { f.closed.Store(true); return nil }

func newService(t *testing.T) (*Service, *conversationstore.SQLiteStore, chan *fakeSession) {
	t.Helper()
	store, err := conversationstore.OpenSQLite(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	opened := make(chan *fakeSession, 4)
	service := NewService(context.Background(), Options{Store: store, Sessions: func(Speaker, string, brain.ActivityObserver) TextSession {
		session := newFakeSession()
		opened <- session
		return session
	}, Logger: quiet})
	return service, store, opened
}

// 说一句：依次 started、text、done；同一条对话上一句还在答时不收；App 断开（Detach）后这一轮照样答完、会话关掉；
// 停止只认这一轮的编号。
func TestSendDetachStop(t *testing.T) {
	service, _, opened := newService(t)
	ctx := context.Background()
	speaker := Speaker{Owner: "主人", Admin: true}
	turn, err := service.Send(ctx, speaker, brain.VoiceThread, Message{Text: "你好"}, "zh")
	if err != nil {
		t.Fatal(err)
	}
	session := <-opened
	if first := <-turn.Events(); first.Type != EventStarted || first.TurnID != turn.ID {
		t.Fatalf("先是 started：%+v", first)
	}
	if text := <-turn.Events(); text.Type != EventText || text.Text != "好的" {
		t.Fatalf("文字：%+v", text)
	}
	if _, err := service.Send(ctx, speaker, brain.VoiceThread, Message{Text: "再说"}, "zh"); !errors.Is(err, ErrBusy) {
		t.Errorf("上一句还在答：%v", err)
	}
	if err := service.Stop(ctx, "主人", brain.VoiceThread, "别的"); !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("编号不对：%v", err)
	}
	turn.Detach()
	if err := service.Stop(ctx, "主人", brain.VoiceThread, turn.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for !session.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("App 断开后这一轮应照样做完、关掉会话")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := service.Send(ctx, speaker, brain.VoiceThread, Message{Text: "接着说"}, "zh"); err != nil {
		t.Errorf("答完了应能接着说：%v", err)
	}
}

// 对话：建、删（语音那条不能删）、删了的不能说、恢复；不是这个人的找不到。
func TestThreadRules(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()
	thread, err := service.Create(ctx, "主人", true)
	if err != nil || !thread.Incognito {
		t.Fatalf("建：%+v %v", thread, err)
	}
	if err := service.Delete(ctx, "主人", brain.VoiceThread); !errors.Is(err, ErrVoiceThread) {
		t.Errorf("语音那条不能删：%v", err)
	}
	if err := service.Delete(ctx, "别人", thread.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("别人的：%v", err)
	}
	if err := service.Delete(ctx, "主人", thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(ctx, Speaker{Owner: "主人"}, thread.ID, Message{Text: "还在吗"}, "zh"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删了的不能说：%v", err)
	}
	if restored, err := service.Restore(ctx, "主人", thread.ID); err != nil || restored.DeletedAt != nil {
		t.Errorf("恢复：%+v %v", restored, err)
	}
}

// fakeRecall 按意思「找到」固定的几轮。
type fakeRecall struct{ seqs []int64 }

func (f fakeRecall) RecallTurns(context.Context, string, string) ([]int64, error) { return f.seqs, nil }

// 搜：按文字找到的和按意思找到的合在一起、去重、新的在前。
func TestSearchMergesTextAndMeaning(t *testing.T) {
	store, err := conversationstore.OpenSQLite(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	record := brain.ConversationRecord{Person: "主人", TurnCount: 3, UpdatedAt: time.Now()}
	for seq, pair := range map[int][2]string{1: {"猫粮买哪家", "A 店"}, 2: {"给猫买吃的", "B 店"}, 3: {"今天天气", "晴"}} {
		record.History = append(record.History, brain.ConversationTurn{Seq: seq, At: time.Unix(int64(1_700_000_000+seq), 0),
			Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: pair[0]}, {Role: brain.RoleAssistant, Text: pair[1]}}})
	}
	slices.SortFunc(record.History, func(a, b brain.ConversationTurn) int { return a.Seq - b.Seq })
	if err := store.SaveConversation(record); err != nil {
		t.Fatal(err)
	}
	service := NewService(context.Background(), Options{Store: store, Recall: fakeRecall{seqs: []int64{2, 1}}, Logger: quiet})
	found, err := service.Search(context.Background(), "主人", "猫粮")
	if err != nil || len(found) != 2 || found[0].Seq != 2 || found[1].Seq != 1 {
		t.Errorf("合在一起、新的在前：%+v %v", found, err)
	}
}
