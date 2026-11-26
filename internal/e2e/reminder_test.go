package e2e

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/reminders"
	"github.com/bukahou/haro/internal/speech"
)

// 提醒的整条路径：调度到点 → NoticeHub 交给这台终端的会话 → 服务端自己开口（不经模型）→ 终端在主循环里播出来 →
// 播完请语音前端醒来（主人可以直接接话）→ 调度记下说过了。终端睡着时也说。
func TestReminderIsSpokenThroughTerminal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 识别替身：不报唤醒、不报说话（chunks 一直不到触发的那块），只回醒 / 睡的请求
	stream := &wakeStream{events: make(chan realtimeproto.ServerEvent, 8), chunks: -1 << 30}
	notices := brain.NewNoticeHub()
	session := brain.NewSession(brain.Components{
		Streaming: &wakeStreaming{stream: stream}, Front: brain.Agent{Model: stubModel{}}, Synthesizer: echoSynthesizer{},
		Notices: notices, NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, logger)
	defer session.Close()

	speaker := &recordingSpeaker{}
	haro := assistant.NewAssistant(assistant.Dependencies{
		Manual: noKeyboard{}, Microphone: loudMicrophone{}, Speaker: speaker, Brain: session,
	}, assistant.Options{TurnDetection: realtimeproto.TurnDetection{Type: "server_vad"}, EchoGuard: 10 * time.Millisecond,
		BargeIn: true, WakeWord: assistant.WakeWordOptions{IdleTimeout: time.Minute}}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = haro.Run(ctx) }()

	store, err := reminders.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 27, 7, 0, 0, 0, time.Local)
	clock := func() time.Time { return now }
	scheduler := reminders.NewScheduler(store, notices, reminders.SchedulerOptions{Now: clock, Retry: 20 * time.Millisecond}, logger)
	if _, err := scheduler.Set(ctx, brain.SharedPerson, reminders.Request{Text: "该喝水了。", After: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute) // 到点（调度循环起来之前拨好钟，Run 里只读）
	go scheduler.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	waitForPlayed(t, speaker, "该喝水了。", deadline, "到点应由服务端开口、终端播出来")
	for {
		listing, _ := scheduler.List(ctx, brain.SharedPerson, false)
		if len(listing.Active) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("说过的一次性提醒应不再在用")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		stream.mu.Lock()
		woke := len(stream.awakeCalls) > 0 && stream.awakeCalls[len(stream.awakeCalls)-1]
		stream.mu.Unlock()
		if woke {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("提醒说完应请语音前端醒来（主人可以直接接话）")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
