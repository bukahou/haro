package chat

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

func (f *fakeSession) SubmitText(text, _, _ string) error {
	f.said = text
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
	service := NewService(store, func(Speaker, string) TextSession {
		session := newFakeSession()
		opened <- session
		return session
	}, quiet)
	return service, store, opened
}

// 说一句：依次 started、text、done；同一条对话上一句还在答时不收；App 断开（Detach）后这一轮照样答完、会话关掉；
// 停止只认这一轮的编号。
func TestSendDetachStop(t *testing.T) {
	service, _, opened := newService(t)
	ctx := context.Background()
	speaker := Speaker{Owner: "主人", Admin: true}
	turn, err := service.Send(ctx, speaker, brain.VoiceThread, "你好", "zh")
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
	if _, err := service.Send(ctx, speaker, brain.VoiceThread, "再说", "zh"); !errors.Is(err, ErrBusy) {
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
	if _, err := service.Send(ctx, speaker, brain.VoiceThread, "接着说", "zh"); err != nil {
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
	if _, err := service.Send(ctx, Speaker{Owner: "主人"}, thread.ID, "还在吗", "zh"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删了的不能说：%v", err)
	}
	if restored, err := service.Restore(ctx, "主人", thread.ID); err != nil || restored.DeletedAt != nil {
		t.Errorf("恢复：%+v %v", restored, err)
	}
}
