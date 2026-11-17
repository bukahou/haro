package brain_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"haro/internal/brain"
	"haro/internal/realtimeproto"
	"haro/internal/speech"
)

// fakeStream 是替身转写会话：记录收到的音频，事件由测试手动发出。
type fakeStream struct {
	mu         sync.Mutex
	audio      [][]byte
	awakeCalls []bool
	events     chan realtimeproto.ServerEvent
	closeOnce  sync.Once
}

func (f *fakeStream) SendAudio(_ context.Context, pcm []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audio = append(f.audio, pcm)
	return nil
}
func (f *fakeStream) SetPlayback(context.Context, bool) error { return nil }
func (f *fakeStream) SetAwake(_ context.Context, awake bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.awakeCalls = append(f.awakeCalls, awake)
	return nil
}
func (f *fakeStream) Events() <-chan realtimeproto.ServerEvent { return f.events }
func (f *fakeStream) Close() error {
	f.closeOnce.Do(func() { close(f.events) })
	return nil
}

type fakeStreaming struct {
	stream    *fakeStream
	gotConfig brain.StreamConfig
}

func (f *fakeStreaming) OpenStream(_ context.Context, config brain.StreamConfig) (brain.RecognizerStream, error) {
	f.gotConfig = config
	return f.stream, nil
}

func nextEvent(t *testing.T, session *brain.Session) realtimeproto.ServerEvent {
	t.Helper()
	select {
	case event := <-session.Events():
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("等事件超时")
		return nil
	}
}

func collectUntilDone(t *testing.T, session *brain.Session) []realtimeproto.ServerEvent {
	t.Helper()
	var events []realtimeproto.ServerEvent
	for {
		event := nextEvent(t, session)
		events = append(events, event)
		if _, done := event.(realtimeproto.ResponseDone); done {
			return events
		}
	}
}

func TestSessionHandsFreeTurn(t *testing.T) {
	streaming := &fakeStreaming{stream: &fakeStream{events: make(chan realtimeproto.ServerEvent, 8)}}
	model := &fakeModel{tokens: []string{"你旁边有一个杯子。"}}
	session := brain.NewSession(brain.Components{
		Recognizer: fakeRecognizer{}, Streaming: streaming, Model: model, Synthesizer: fakeSynthesizer{},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer session.Close()
	ctx := context.Background()

	turn := realtimeproto.TurnDetection{Type: "server_vad", SilenceDurationMs: 700}
	if err := session.Send(ctx, realtimeproto.SessionUpdate{Instructions: "测试", TurnDetection: &turn}); err != nil {
		t.Fatal(err)
	}
	if streaming.gotConfig.TurnDetection.SilenceDurationMs != 700 {
		t.Errorf("转写会话收到的配置 %+v", streaming.gotConfig)
	}
	// 音频直接转发；格式不对拒绝；手动提交拒绝
	chunk := realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: make([]byte, 640)}
	if err := session.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: chunk}); err != nil {
		t.Fatal(err)
	}
	if len(streaming.stream.audio) != 1 {
		t.Errorf("音频没有转发给转写会话")
	}
	wrong := realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1}, Data: []byte{0, 0}}
	if err := session.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: wrong}); err == nil {
		t.Error("格式不对的音频应被拒绝")
	}
	if err := session.Send(ctx, realtimeproto.InputAudioBufferCommit{}); err == nil {
		t.Error("免按键模式下手动提交应被拒绝")
	}

	// VAD：开始 → 结束；终端带画面请求回答；识别结果随后才到
	streaming.stream.events <- realtimeproto.InputAudioBufferSpeechStarted{AudioStartMs: 100, ItemID: "item_1"}
	streaming.stream.events <- realtimeproto.InputAudioBufferSpeechStopped{AudioEndMs: 900, ItemID: "item_1"}
	if _, ok := nextEvent(t, session).(realtimeproto.InputAudioBufferSpeechStarted); !ok {
		t.Fatal("应先转发 speech_started")
	}
	if stopped, ok := nextEvent(t, session).(realtimeproto.InputAudioBufferSpeechStopped); !ok || stopped.AudioEndMs != 900 {
		t.Fatal("应转发 speech_stopped")
	}
	_ = session.Send(ctx, realtimeproto.ConversationItemCreate{Item: realtimeproto.ConversationItem{Role: "user",
		Content: []realtimeproto.ContentPart{{Type: "input_image", ImageJPEG: []byte("jpeg")}}}})
	if err := session.Send(ctx, realtimeproto.ResponseCreate{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // 回答应在等识别结果
	streaming.stream.events <- realtimeproto.InputAudioTranscriptionCompleted{ItemID: "item_1", Transcript: " 我旁边有什么？ "}

	events := collectUntilDone(t, session)
	if done := events[len(events)-1].(realtimeproto.ResponseDone); done.Status != realtimeproto.StatusCompleted {
		t.Fatalf("response.done = %+v（事件 %v）", done, eventTypes(events))
	}
	if model.lastInput.Text != "我旁边有什么？" || model.lastInput.Image != nil { // 画面按需（look_at_camera）
		t.Errorf("模型收到 %+v", model.lastInput)
	}

	// 连接断开：等识别结果的回答应失败，而不是卡住
	streaming.stream.events <- realtimeproto.InputAudioBufferSpeechStopped{AudioEndMs: 2000, ItemID: "item_2"}
	nextEvent(t, session) // speech_stopped
	_ = session.Send(ctx, realtimeproto.ResponseCreate{})
	streaming.stream.Close()
	events = collectUntilDone(t, session)
	if done := events[len(events)-1].(realtimeproto.ResponseDone); done.Status != realtimeproto.StatusFailed {
		t.Errorf("断开后 response.done = %+v", done)
	}
	if !slices.Contains(eventTypes(events), "error") {
		t.Errorf("断开后应有 error 事件：%v", eventTypes(events))
	}
}

func TestSessionHandsFreeWithoutStreaming(t *testing.T) {
	session := newTestSession(fakeRecognizer{}, &fakeModel{}, fakeSynthesizer{})
	defer session.Close()
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	if err := session.Send(context.Background(), realtimeproto.SessionUpdate{TurnDetection: &turn}); err == nil {
		t.Error("没有配置流式识别时应拒绝免按键模式")
	}
}

func TestSessionWakeWordAcknowledgement(t *testing.T) {
	streaming := &fakeStreaming{stream: &fakeStream{events: make(chan realtimeproto.ServerEvent, 8)}}
	model := &fakeModel{tokens: []string{"不应该调用模型。"}}
	session := brain.NewSession(brain.Components{
		Recognizer: fakeRecognizer{}, Streaming: streaming, Model: model, Synthesizer: fakeSynthesizer{},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer session.Close()
	ctx := context.Background()
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	if err := session.Send(ctx, realtimeproto.SessionUpdate{TurnDetection: &turn, WakeWord: true}); err != nil {
		t.Fatal(err)
	}
	if !streaming.gotConfig.WakeWord {
		t.Error("转写会话应启用唤醒门")
	}
	// 唤醒门控制转发给转写会话
	_ = session.Send(ctx, realtimeproto.HaroWakeWordSleep{})
	_ = session.Send(ctx, realtimeproto.HaroWakeWordWake{})
	if got := streaming.stream.awakeCalls; len(got) != 2 || got[0] || !got[1] {
		t.Errorf("SetAwake 调用 %v，应为 [false true]", got)
	}

	// 只叫了唤醒词：识别结果只有标点 → 回「我在。」，不调用大模型
	streaming.stream.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗", AudioMs: 1000}
	streaming.stream.events <- realtimeproto.InputAudioBufferSpeechStopped{AudioEndMs: 1000, ItemID: "item_1"}
	nextEvent(t, session) // wake_word.detected
	nextEvent(t, session) // speech_stopped
	_ = session.Send(ctx, realtimeproto.ResponseCreate{})
	streaming.stream.events <- realtimeproto.InputAudioTranscriptionCompleted{ItemID: "item_1", Transcript: "。"}
	events := collectUntilDone(t, session)
	var spoken string
	for _, event := range events {
		if audio, ok := event.(realtimeproto.ResponseOutputAudioDelta); ok {
			spoken += string(audio.Audio.Data)
		}
	}
	if done := events[len(events)-1].(realtimeproto.ResponseDone); done.Status != realtimeproto.StatusCompleted || spoken != "我在。" {
		t.Errorf("应回「我在。」：播出 %q，done %+v", spoken, done)
	}
	if model.lastInput.Text != "" {
		t.Error("只叫唤醒词时不应调用大模型")
	}

	// 之后没叫唤醒词的空识别：照常是 incomplete
	streaming.stream.events <- realtimeproto.InputAudioBufferSpeechStopped{AudioEndMs: 3000, ItemID: "item_2"}
	nextEvent(t, session)
	_ = session.Send(ctx, realtimeproto.ResponseCreate{})
	streaming.stream.events <- realtimeproto.InputAudioTranscriptionCompleted{ItemID: "item_2", Transcript: ""}
	events = collectUntilDone(t, session)
	if done := events[len(events)-1].(realtimeproto.ResponseDone); done.Status != realtimeproto.StatusIncomplete {
		t.Errorf("未唤醒的空识别应为 incomplete：%+v", done)
	}
}

// 声纹（v3 H-3）：识别结果标着「比对过、没认出来」⇒ 不回答；认出来了照常回答。
func TestUnknownSpeakerIsIgnored(t *testing.T) {
	streaming := &fakeStreaming{stream: &fakeStream{events: make(chan realtimeproto.ServerEvent, 8)}}
	model := &fakeModel{tokens: []string{"好的。"}}
	session := brain.NewSession(brain.Components{
		Recognizer: fakeRecognizer{}, Streaming: streaming, Model: model, Synthesizer: fakeSynthesizer{},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer session.Close()
	ctx := context.Background()
	turn := realtimeproto.TurnDetection{Type: "server_vad", SilenceDurationMs: 700}
	_ = session.Send(ctx, realtimeproto.SessionUpdate{TurnDetection: &turn})

	ask := func(item, speaker string) realtimeproto.ResponseDone {
		streaming.stream.events <- realtimeproto.InputAudioBufferSpeechStopped{AudioEndMs: 900, ItemID: item}
		nextEvent(t, session) // speech_stopped
		_ = session.Send(ctx, realtimeproto.ResponseCreate{})
		streaming.stream.events <- realtimeproto.InputAudioTranscriptionCompleted{ItemID: item, Transcript: "你好",
			Speaker: speaker, SpeakerChecked: true}
		events := collectUntilDone(t, session)
		return events[len(events)-1].(realtimeproto.ResponseDone)
	}
	if done := ask("item_1", ""); done.Status != realtimeproto.StatusIncomplete || done.Reason != "unknown_speaker" {
		t.Errorf("陌生人的话不应回答：%+v", done)
	}
	if done := ask("item_2", "主人"); done.Status != realtimeproto.StatusCompleted {
		t.Errorf("主人的话应照常回答：%+v", done)
	}
}

// v4 D2：两台终端同时听到唤醒词——只有质量高的那台把唤醒告诉终端，另一台的识别流被送回休眠。
func TestTwoTerminalsHearingWakeWordOnlyOneAnswers(t *testing.T) {
	arbiter := brain.NewWakeArbiter(50 * time.Millisecond)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func() (*brain.Session, *fakeStream) {
		stream := &fakeStream{events: make(chan realtimeproto.ServerEvent, 8)}
		session := brain.NewSession(brain.Components{
			Recognizer: fakeRecognizer{}, Streaming: &fakeStreaming{stream: stream}, Model: &fakeModel{},
			Synthesizer: fakeSynthesizer{}, WakeArbiter: arbiter,
			NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
		}, brain.Options{MaxHistoryTurn: 6}, logger)
		turn := realtimeproto.TurnDetection{Type: "server_vad"}
		if err := session.Send(context.Background(), realtimeproto.SessionUpdate{TurnDetection: &turn, WakeWord: true}); err != nil {
			t.Fatal(err)
		}
		return session, stream
	}
	near, nearStream := open()
	defer near.Close()
	far, farStream := open()
	defer far.Close()

	farStream.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗", Score: 0.30, Level: 0.01}
	nearStream.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗", Score: 0.55, Level: 0.04}

	if _, ok := nextEvent(t, near).(realtimeproto.HaroWakeWordDetected); !ok {
		t.Fatal("近的那台应收到唤醒")
	}
	select {
	case event := <-far.Events():
		t.Fatalf("远的那台不该收到任何事件：%v", event)
	case <-time.After(200 * time.Millisecond):
	}
	farStream.mu.Lock()
	defer farStream.mu.Unlock()
	if len(farStream.awakeCalls) != 1 || farStream.awakeCalls[0] {
		t.Errorf("远的那台的识别流应被送回休眠：%v", farStream.awakeCalls)
	}
}
