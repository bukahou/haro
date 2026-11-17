package assistant_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"haro/internal/assistant"
	"haro/internal/brain"
	"haro/internal/realtimeproto"
	"haro/internal/speech"
)

// 唤醒词整轮：休眠 → 听到唤醒词 → 一口气说完的问题被回答 → 对话窗口超时 → 通知休眠。

type blockingManual struct{ done chan struct{} }

func (b blockingManual) WaitForToggle(ctx context.Context) error {
	select {
	case <-b.done:
		return io.EOF // 模拟终端输入结束，让 Run 退出
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wakeStream 是替身转写会话：收到第 10 块音频时报告「唤醒 + 一句话」，并记录 SetAwake。
type wakeStream struct {
	mu         sync.Mutex
	chunks     int
	awakeCalls []bool
	events     chan realtimeproto.ServerEvent
	closeOnce  sync.Once
}

func (w *wakeStream) SendAudio(context.Context, []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.chunks++
	if w.chunks == 10 {
		w.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗", AudioMs: 100}
		w.events <- realtimeproto.InputAudioBufferSpeechStarted{AudioStartMs: 100, ItemID: "item_1"}
		w.events <- realtimeproto.InputAudioBufferSpeechStopped{AudioEndMs: 200, ItemID: "item_1"}
		w.events <- realtimeproto.InputAudioTranscriptionCompleted{ItemID: "item_1", Transcript: "我旁边有什么"}
	}
	return nil
}
func (w *wakeStream) SetPlayback(context.Context, bool) error { return nil }
func (w *wakeStream) SetAwake(_ context.Context, awake bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.awakeCalls = append(w.awakeCalls, awake)
	return nil
}
func (w *wakeStream) Events() <-chan realtimeproto.ServerEvent { return w.events }
func (w *wakeStream) Close() error {
	w.closeOnce.Do(func() { close(w.events) })
	return nil
}
func (w *wakeStream) sleepRequested() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.awakeCalls) > 0 && !w.awakeCalls[len(w.awakeCalls)-1]
}

type wakeStreaming struct {
	stream    *wakeStream
	gotConfig brain.StreamConfig
}

func (w *wakeStreaming) OpenStream(_ context.Context, config brain.StreamConfig) (brain.RecognizerStream, error) {
	w.gotConfig = config
	return w.stream, nil
}

func TestWakeWordConversation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	streaming := &wakeStreaming{stream: &wakeStream{events: make(chan realtimeproto.ServerEvent, 8)}}
	notices := brain.NewNoticeHub()
	session := brain.NewSession(brain.Components{
		Recognizer: stubRecognizer{}, Streaming: streaming, Model: stubModel{}, Synthesizer: echoSynthesizer{}, Notices: notices,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, logger)
	defer session.Close()

	manual := blockingManual{done: make(chan struct{})}
	speaker := &recordingSpeaker{}
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	haro := assistant.NewAssistant(assistant.Components{
		Manual: manual, Microphone: loudMicrophone{}, Camera: fakeCamera{}, Speaker: speaker, Brain: session,
	}, assistant.Options{TurnDetection: &turn, EchoGuard: 10 * time.Millisecond,
		NoticeTone: realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: []byte("[叮咚]")},
		WakeWord: &assistant.WakeWordOptions{Phrase: "哈罗哈罗", IdleTimeout: 150 * time.Millisecond,
			SleepTone: realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: []byte("[我去睡觉了]")}}}, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- haro.Run(ctx) }()

	// 唤醒后一口气说完的问题被回答；回答播完 150ms 后应通知休眠
	deadline := time.Now().Add(3 * time.Second)
	for !streaming.stream.sleepRequested() {
		if time.Now().After(deadline) {
			t.Fatal("对话窗口结束后没有通知休眠")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !streaming.gotConfig.WakeWord {
		t.Error("应以唤醒门模式打开转写会话")
	}
	// 回答播完、对话窗口结束：先通知休眠，再说一声「我去睡觉了」
	want := "你旁边有一个杯子。还有键盘。[我去睡觉了]"
	for _, audio, _ := speaker.snapshot(); audio != want; _, audio, _ = speaker.snapshot() {
		if time.Now().After(deadline) {
			t.Fatalf("唤醒后的问题应被回答、休眠时说一声：播出 %q", audio)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 睡着时大脑发来提示（后台调查查完了）：响一声
	notices.Notify(brain.SharedPerson, realtimeproto.HaroNotice{Kind: realtimeproto.NoticeResearchDone})
	for _, audio, _ := speaker.snapshot(); audio != want+"[叮咚]"; _, audio, _ = speaker.snapshot() {
		if time.Now().After(deadline) {
			t.Fatalf("收到提示应响一声：播出 %q", audio)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 键盘输入结束（作为服务运行时 stdin 是空的）：不退出，照常靠唤醒词运行
	close(manual.done)
	select {
	case err := <-runErr:
		t.Fatalf("没有键盘不应退出：%v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run 返回 %v，应为 context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后 Run 没有退出")
	}
}
