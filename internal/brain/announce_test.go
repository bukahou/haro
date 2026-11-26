package brain

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// textSynth 把句子原样放进 PCM；fail 为 true 时报错（语音合成用不了）。
type textSynth struct{ fail bool }

func (s textSynth) Synthesize(_ context.Context, utterance Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		if s.fail {
			yield(realtimeproto.PCMAudio{}, errors.New("合成服务连不上"))
			return
		}
		yield(realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: []byte(utterance.Text)}, nil)
	}
}

func newAnnounceSession(t *testing.T, hub *NoticeHub, synth SpeechSynthesizer) *Session {
	t.Helper()
	s := NewSession(Components{Notices: hub, Synthesizer: synth}, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// outcomes 收集 Done 的结果。
type outcomes chan AnnouncementOutcome

func (o outcomes) done(outcome AnnouncementOutcome) { o <- outcome }

func (o outcomes) wait(t *testing.T) AnnouncementOutcome {
	t.Helper()
	select {
	case outcome := <-o:
		return outcome
	case <-time.After(2 * time.Second):
		t.Fatal("开口一直没有结果")
		return 0
	}
}

func nextEvent(t *testing.T, s *Session) realtimeproto.ServerEvent {
	t.Helper()
	select {
	case event := <-s.Events():
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("等不到事件")
		return nil
	}
}

func noEvent(t *testing.T, s *Session, what string) {
	t.Helper()
	select {
	case event := <-s.Events():
		t.Fatalf("%s：不应有事件，收到 %s", what, event.Type())
	case <-time.After(50 * time.Millisecond):
	}
}

// 空闲时马上开口：恰好一个 response.created（带上缘由）、文字和声音、一个 response.done；说出的话连同说明记进这个人的对话。
func TestAnnouncementSpeaksWhenIdle(t *testing.T) {
	hub := NewNoticeHub()
	s := newAnnounceSession(t, hub, textSynth{})
	results := make(outcomes, 1)
	if !hub.Announce(Announcement{Reason: realtimeproto.UnpromptedReminder, Person: "主人", Text: "[温柔]该喝水了。",
		Note: "到点提醒 #1", Done: results.done}) {
		t.Fatal("有在线的终端，应交得出去")
	}
	created, ok := nextEvent(t, s).(realtimeproto.ResponseCreated)
	if !ok || created.Unprompted != realtimeproto.UnpromptedReminder {
		t.Fatalf("应先发带缘由的 response.created：%+v", created)
	}
	if delta, ok := nextEvent(t, s).(realtimeproto.ResponseOutputAudioTranscriptDelta); !ok || delta.Delta != "该喝水了。" {
		t.Errorf("文字（去掉情绪标记）：%+v", delta)
	}
	if _, ok := nextEvent(t, s).(realtimeproto.ResponseOutputAudioDelta); !ok {
		t.Error("应有声音")
	}
	if done, ok := nextEvent(t, s).(realtimeproto.ResponseDone); !ok || done.Status != realtimeproto.StatusCompleted {
		t.Errorf("应以 completed 的 response.done 结束：%+v", done)
	}
	if outcome := results.wait(t); outcome != AnnouncementSpoken {
		t.Errorf("结果：%s", outcomeLabel(outcome))
	}
	messages := s.parts.Conversations.Conversation("主人").messages()
	n := len(messages)
	if n < 2 || !messages[n-2].Synthetic || messages[n-2].Text != situationHeading+"到点提醒 #1" ||
		messages[n-1].Role != RoleAssistant || messages[n-1].Text != "该喝水了。" {
		t.Errorf("应记进主人的对话（说明 + 说出的话）：%+v", messages)
	}
}

// 终端还在播（上一轮的回答）时不开口：等它报播完再说，声音不会混在一起。
func TestAnnouncementWaitsForPlaybackToStop(t *testing.T) {
	hub := NewNoticeHub()
	s := newAnnounceSession(t, hub, textSynth{})
	ctx := context.Background()
	_ = s.Send(ctx, realtimeproto.HaroPlaybackStarted{})
	results := make(outcomes, 1)
	hub.Announce(Announcement{Reason: realtimeproto.UnpromptedReminder, Person: "主人", Text: "该喝水了。", Done: results.done})
	noEvent(t, s, "终端在播")
	_ = s.Send(ctx, realtimeproto.HaroPlaybackStopped{})
	if _, ok := nextEvent(t, s).(realtimeproto.ResponseCreated); !ok {
		t.Fatal("终端播完后应开口")
	}
	if outcome := results.wait(t); outcome != AnnouncementSpoken {
		t.Errorf("结果：%s", outcomeLabel(outcome))
	}
}

// 主人正在说话时不开口（开口会打断他）：这句说完、答完以后再说。
func TestAnnouncementWaitsWhileOwnerSpeaks(t *testing.T) {
	hub := NewNoticeHub()
	s := newAnnounceSession(t, hub, textSynth{})
	s.mu.Lock()
	s.speech = &realtimeproto.InputAudioBufferSpeechStarted{ItemID: "item_1"}
	s.mu.Unlock()
	hub.Announce(Announcement{Reason: realtimeproto.UnpromptedReminder, Person: "主人", Text: "该喝水了。"})
	noEvent(t, s, "主人在说话")
	s.mu.Lock()
	s.speech = nil
	s.startAnnouncementLocked() // 说完（这里直接模拟：没有回答要等）
	s.mu.Unlock()
	if _, ok := nextEvent(t, s).(realtimeproto.ResponseCreated); !ok {
		t.Fatal("主人说完后应开口")
	}
}

// 语音合成用不了：这次开口以失败结束，改让终端播提醒的提示音（D2 c）；没说出来就不记进历史。
func TestAnnouncementFallsBackToChime(t *testing.T) {
	hub := NewNoticeHub()
	s := newAnnounceSession(t, hub, textSynth{fail: true})
	results := make(outcomes, 1)
	hub.Announce(Announcement{Reason: realtimeproto.UnpromptedReminder, Person: "主人", Text: "该喝水了。", Done: results.done})
	var sawFailed, sawChime bool
	for !sawChime {
		switch e := nextEvent(t, s).(type) {
		case realtimeproto.ResponseDone:
			sawFailed = e.Status == realtimeproto.StatusFailed
		case realtimeproto.HaroNotice:
			sawChime = e.Kind == realtimeproto.NoticeReminder
		}
	}
	if !sawFailed {
		t.Error("合成失败的开口应以 failed 结束（在提示音之前）")
	}
	if outcome := results.wait(t); outcome != AnnouncementChimed {
		t.Errorf("结果：%s", outcomeLabel(outcome))
	}
	if messages := s.parts.Conversations.Conversation("主人").messages(); len(messages) != 0 {
		t.Errorf("没说出来的不记进历史：%+v", messages)
	}
}

// 会话关了：排着没说的算没能说（调度之后再试）；没有在线的终端时交不出去。
func TestAnnouncementDroppedOnClose(t *testing.T) {
	hub := NewNoticeHub()
	if hub.Announce(Announcement{Person: "主人", Text: "该喝水了。"}) {
		t.Fatal("没有在线的终端时应交不出去")
	}
	s := newAnnounceSession(t, hub, textSynth{})
	_ = s.Send(context.Background(), realtimeproto.HaroPlaybackStarted{}) // 在播：排着
	results := make(outcomes, 1)
	hub.Announce(Announcement{Person: "主人", Text: "该喝水了。", Done: results.done})
	_ = s.Close()
	if outcome := results.wait(t); outcome != AnnouncementDropped {
		t.Errorf("结果：%s", outcomeLabel(outcome))
	}
}

// 送到这个人最近说话的那台；他在哪台都没说过话时交给最近连上的那一台，只说一遍。
func TestAnnouncementGoesToOneTerminal(t *testing.T) {
	hub := NewNoticeHub()
	kitchen := newAnnounceSession(t, hub, textSynth{})
	time.Sleep(time.Millisecond)
	bedroom := newAnnounceSession(t, hub, textSynth{}) // 后连上的
	hub.Announce(Announcement{Person: "主人", Text: "该喝水了。"})
	if _, ok := nextEvent(t, bedroom).(realtimeproto.ResponseCreated); !ok {
		t.Fatal("没在任何终端说过话：交给最近连上的卧室")
	}
	noEvent(t, kitchen, "厨房")
	kitchen.useConversationOf("主人")    // 主人在厨房说了话
	time.Sleep(100 * time.Millisecond) // 卧室那句说完
	for len(bedroom.Events()) > 0 {
		<-bedroom.Events()
	}
	hub.Announce(Announcement{Person: "主人", Text: "该吃药了。"})
	if _, ok := nextEvent(t, kitchen).(realtimeproto.ResponseCreated); !ok {
		t.Fatal("应送到主人最近说话的厨房")
	}
	noEvent(t, bedroom, "卧室")
}

// 有终端连上时通知登记的那一方（提醒的调度据此看看有没有等着终端的提醒）。
func TestNoticeHubReportsTerminalJoined(t *testing.T) {
	hub := NewNoticeHub()
	joined := make(chan struct{}, 2)
	hub.OnTerminalJoined(func() { joined <- struct{}{} })
	newAnnounceSession(t, hub, textSynth{})
	select {
	case <-joined:
	default:
		t.Fatal("终端连上时应通知")
	}
}
