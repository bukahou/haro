package brain_test

import (
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

// textTurn 在打字会话里说一句，收事件直到这一轮结束。
func textTurn(t *testing.T, session *brain.Session, text, person string) []realtimeproto.ServerEvent {
	t.Helper()
	if err := session.SubmitText(text, person, "zh"); err != nil {
		t.Fatal(err)
	}
	var events []realtimeproto.ServerEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-session.Events():
			events = append(events, event)
			if _, done := event.(realtimeproto.ResponseDone); done {
				return events
			}
		case <-timeout:
			t.Fatalf("这一轮没结束：%v", events)
		}
	}
}

// 打字会话：不开识别流、不合成语音，回答以文字发出；接在指定的线程里（不碰语音那条），同一个人的编号共用一套；
// 上一句还在答时不收下一句；语音会话不收打字的话。
func TestTextSession(t *testing.T) {
	store, err := conversationstore.OpenSQLite(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	book := brain.NewConversationBook(store, brain.Options{MaxHistoryTurn: 6}, quiet)
	model := &fakeModel{tokens: []string{"好的，", "记下了。"}}
	parts := brain.Components{Front: withTestSteps(brain.Agent{Model: model}), Conversations: book, Thread: "t1", TextOnly: true,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }}
	session := brain.NewSession(parts, brain.Options{MaxHistoryTurn: 6}, quiet)
	t.Cleanup(func() { _ = session.Close() })
	events := textTurn(t, session, "明天下雨吗", "主人")
	if done := events[len(events)-1].(realtimeproto.ResponseDone); done.Status != realtimeproto.StatusCompleted {
		t.Fatalf("这一轮：%+v", done)
	}
	text := ""
	for _, event := range events {
		switch e := event.(type) {
		case realtimeproto.ResponseOutputAudioTranscriptDelta:
			text += e.Delta
		case realtimeproto.ResponseOutputAudioDelta:
			t.Error("打字会话不该合成语音")
		}
	}
	if text != "好的，记下了。" || model.lastInput.Text == "" {
		t.Errorf("回答的文字：%q（模型收到 %q）", text, model.lastInput.Text)
	}
	if record, ok, _ := store.LoadConversation("主人", "t1"); !ok || len(record.History) != 1 || record.History[0].Seq != 1 {
		t.Errorf("这一轮应记在线程 t1 里：%+v", record)
	}
	if _, ok, _ := store.LoadConversation("主人", brain.VoiceThread); ok {
		t.Error("语音那条不该被碰")
	}
	voice := brain.NewSession(brain.Components{Front: withTestSteps(brain.Agent{Model: model}), Conversations: book},
		brain.Options{MaxHistoryTurn: 6}, quiet)
	t.Cleanup(func() { _ = voice.Close() })
	if err := voice.SubmitText("你好", "主人", "zh"); err == nil {
		t.Error("语音会话不该收打字的话")
	}
	slow := &fakeModel{tokens: []string{"慢", "慢", "答"}, delay: 200 * time.Millisecond}
	busy := brain.NewSession(brain.Components{Front: withTestSteps(brain.Agent{Model: slow}), Conversations: book, Thread: "t2", TextOnly: true,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }}, brain.Options{MaxHistoryTurn: 6}, quiet)
	t.Cleanup(func() { _ = busy.Close() })
	if err := busy.SubmitText("第一句", "主人", "zh"); err != nil {
		t.Fatal(err)
	}
	if err := busy.SubmitText("第二句", "主人", "zh"); err == nil {
		t.Error("上一句还在答时不该收下一句")
	}
}
