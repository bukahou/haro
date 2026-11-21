package brain_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

func newStreamSession(t *testing.T, voice *braintest.Voice, model brain.ChatModel, arbiter *brain.WakeArbiter) *brain.Session {
	t.Helper()
	parts := brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, WakeArbiter: arbiter,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }}
	if voice != nil {
		parts.Streaming = voice
	}
	session := brain.NewSession(parts, brain.Options{MaxHistoryTurn: 6}, quiet)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestSessionHandsFreeTurn(t *testing.T) {
	voice := braintest.NewVoice()
	model := &fakeModel{tokens: []string{"你旁边有一个杯子。"}}
	session := newStreamSession(t, voice, model, nil)
	ctx := context.Background()

	turn := realtimeproto.TurnDetection{Type: "server_vad", SilenceDurationMs: 700}
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn})
	if voice.Config().TurnDetection.SilenceDurationMs != 700 {
		t.Errorf("转写会话收到的配置 %+v", voice.Config())
	}
	// 音频直接转发；格式不对拒绝；手动提交拒绝
	chunk := realtimeproto.PCMAudio{Format: testFormat, Data: make([]byte, 640)}
	if err := session.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: chunk}); err != nil {
		t.Fatal(err)
	}
	if len(stream.Audio()) != 1 {
		t.Errorf("音频没有转发给转写会话")
	}
	wrong := realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1}, Data: []byte{0, 0}}
	if err := session.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: wrong}); err == nil {
		t.Error("格式不对的音频应被拒绝")
	}

	// VAD：开始 → 结束；服务端判定说完时自己开始回答（不等终端请求）；识别结果随后才到
	braintest.Speak(t, session, stream, braintest.Utterance{})
	braintest.Expect[realtimeproto.ResponseCreated](t, session)
	time.Sleep(20 * time.Millisecond) // 回答应在等识别结果
	braintest.Recognized(t, stream, braintest.Utterance{Text: " 我旁边有什么？ "})

	events := braintest.CollectUntilDone(t, session)
	if done := braintest.Done(t, events); done.Status != realtimeproto.StatusCompleted {
		t.Fatalf("response.done = %+v（事件 %v）", done, braintest.Types(events))
	}
	if model.lastInput.Text != "我旁边有什么？" || model.lastInput.Image != nil { // 画面按需（look_at_camera）
		t.Errorf("模型收到 %+v", model.lastInput)
	}

	// 连接断开：等识别结果的回答应失败，而不是卡住（识别流随后自己重开，见 TestStreamReopensAfterDisconnect）
	braintest.Speak(t, session, stream, braintest.Utterance{})
	braintest.Expect[realtimeproto.ResponseCreated](t, session)
	_ = stream.Close()
	events = braintest.CollectUntilDone(t, session)
	if done := braintest.Done(t, events); done.Status != realtimeproto.StatusFailed || done.Reason != realtimeproto.ReasonTranscriptionFailed {
		t.Errorf("断开后 response.done = %+v", done)
	}
}

func newReopenSession(t *testing.T, voice *braintest.Voice, timing brain.ReopenTiming) *brain.Session {
	t.Helper()
	parts := brain.Components{Front: brain.Agent{Model: &fakeModel{tokens: []string{"好的。"}}}, Synthesizer: fakeSynthesizer{}, Streaming: voice,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }}
	session := brain.NewSession(parts, brain.Options{MaxHistoryTurn: 6, StreamReopen: timing}, quiet)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// waitOpened 等识别流被打开到第 n 条。
func waitOpened(t *testing.T, voice *braintest.Voice, n int) *braintest.Stream {
	t.Helper()
	deadline := time.Now().Add(braintest.EventTimeout)
	for voice.Opened() < n {
		if time.Now().After(deadline) {
			t.Fatalf("应重开识别流：打开了 %d 条，等 %d 条", voice.Opened(), n)
		}
		time.Sleep(time.Millisecond)
	}
	return voice.Stream()
}

// 识别流断开：按原来的设置自己重开，把会话记着的「醒着 / 正在播放」告诉新的流，之后照常对话。
// 断开期间送来的音频丢掉、不逐块回错误；断开很快恢复时不提示终端。
func TestStreamReopensAfterDisconnect(t *testing.T) {
	voice := braintest.NewVoice()
	session := newReopenSession(t, voice, brain.ReopenTiming{Delay: time.Millisecond, MaxDelay: 5 * time.Millisecond,
		LostNoticeAfter: time.Hour})
	ctx := context.Background()
	turn := realtimeproto.TurnDetection{Type: "server_vad", SilenceDurationMs: 600}
	first := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn, WakeWord: true})
	_ = session.Send(ctx, realtimeproto.HaroWakeWordWake{})
	_ = session.Send(ctx, realtimeproto.HaroPlaybackStarted{})

	voice.FailNextOpen(errors.New("连不上 haro-asr"))
	_ = first.Close()
	chunk := realtimeproto.PCMAudio{Format: testFormat, Data: make([]byte, 640)}
	time.Sleep(20 * time.Millisecond) // 让它重试几次
	for range 3 {
		if err := session.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: chunk}); err != nil {
			t.Fatalf("断开期间的音频应悄悄丢掉：%v", err)
		}
	}
	_ = session.Send(ctx, realtimeproto.HaroPlaybackStopped{}) // 断开期间的状态变化也要记下
	voice.FailNextOpen(nil)

	second := waitOpened(t, voice, 2)
	if voice.Config().TurnDetection.SilenceDurationMs != 600 || !voice.Config().WakeWord {
		t.Errorf("应按原来的设置重开：%+v", voice.Config())
	}
	deadline := time.Now().Add(braintest.EventTimeout)
	for len(second.AwakeCalls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := second.AwakeCalls(); !slices.Equal(got, []bool{true}) {
		t.Errorf("重开后应恢复醒着：%v", got)
	}
	if got := second.PlaybackCalls(); len(got) != 0 {
		t.Errorf("断开期间播放已经停了，重开后不应再说在播放：%v", got)
	}
	if len(second.Audio()) != 0 {
		t.Errorf("断开期间的音频不应补发：%d 块", len(second.Audio()))
	}

	if done := braintest.Done(t, braintest.Ask(t, session, second, braintest.Utterance{Text: "你好"})); done.Status != realtimeproto.StatusCompleted {
		t.Errorf("重开后应照常对话：%+v", done)
	}
	select {
	case event := <-session.Events():
		t.Errorf("断开很快恢复时不应再有别的事件（提示、错误）：%+v", event)
	default:
	}
}

// 断开超过 LostNoticeAfter 还没重开：提示终端一次「听不见了」（不重复），重开后再提示一次「恢复了」。
func TestStreamLostAndRestoredNotices(t *testing.T) {
	voice := braintest.NewVoice()
	session := newReopenSession(t, voice, brain.ReopenTiming{Delay: time.Millisecond, MaxDelay: 2 * time.Millisecond,
		LostNoticeAfter: 20 * time.Millisecond})
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	first := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn})

	voice.FailNextOpen(errors.New("连不上 haro-asr"))
	_ = first.Close()
	if notice := braintest.Expect[realtimeproto.HaroNotice](t, session); notice.Kind != realtimeproto.NoticeTranscriptionLost {
		t.Fatalf("断开太久应提示听不见：%+v", notice)
	}
	time.Sleep(50 * time.Millisecond) // 继续重试失败：不应再提示
	voice.FailNextOpen(nil)
	if notice := braintest.Expect[realtimeproto.HaroNotice](t, session); notice.Kind != realtimeproto.NoticeTranscriptionRestored {
		t.Fatalf("重开后应提示恢复（中间不应有重复的断开提示）：%+v", notice)
	}
	waitOpened(t, voice, 2)
}

// 服务端先起来、haro-asr 还没好：打开识别流失败时会话照常建立（不回错误），在后台接着连，连上之后照常对话。
func TestStreamOpensLaterWhenRecognizerIsNotReady(t *testing.T) {
	voice := braintest.NewVoice()
	session := newReopenSession(t, voice, brain.ReopenTiming{Delay: time.Millisecond, MaxDelay: 2 * time.Millisecond,
		LostNoticeAfter: time.Hour})
	ctx := context.Background()
	voice.FailNextOpen(errors.New("连不上 haro-asr（还在加载模型）"))
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	if err := session.Send(ctx, realtimeproto.SessionUpdate{TurnDetection: &turn}); err != nil {
		t.Fatalf("识别服务暂时连不上时，session.update 不应失败：%v", err)
	}
	chunk := realtimeproto.PCMAudio{Format: testFormat, Data: make([]byte, 640)}
	if err := session.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: chunk}); err != nil {
		t.Fatalf("连上之前的音频应悄悄丢掉：%v", err)
	}
	time.Sleep(10 * time.Millisecond)
	voice.FailNextOpen(nil)
	stream := waitOpened(t, voice, 1)
	if done := braintest.Done(t, braintest.Ask(t, session, stream, braintest.Utterance{Text: "你好"})); done.Status != realtimeproto.StatusCompleted {
		t.Errorf("连上之后应照常对话：%+v", done)
	}
}

// 会话结束时不再重开。
func TestStreamNotReopenedAfterClose(t *testing.T) {
	voice := braintest.NewVoice()
	session := newReopenSession(t, voice, brain.ReopenTiming{Delay: time.Millisecond, MaxDelay: time.Millisecond, LostNoticeAfter: time.Hour})
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn})
	_ = session.Close()
	time.Sleep(20 * time.Millisecond)
	if voice.Opened() != 1 {
		t.Errorf("会话结束后不应重开识别流：打开了 %d 条", voice.Opened())
	}
}

func TestServerVADRejectedWithoutStreaming(t *testing.T) {
	session := newStreamSession(t, nil, &fakeModel{}, nil)
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	if err := session.Send(context.Background(), realtimeproto.SessionUpdate{TurnDetection: &turn}); err == nil {
		t.Error("没有配置流式识别时应拒绝 server_vad")
	}
}

// 唤醒门控制转发给转写会话；只有标点的识别结果不回答（叫没叫唤醒词都一样）。
func TestSessionWakeGateAndEmptyTranscript(t *testing.T) {
	voice := braintest.NewVoice()
	model := &fakeModel{tokens: []string{"不应该调用模型。"}}
	session := newStreamSession(t, voice, model, nil)
	ctx := context.Background()
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn, WakeWord: true})
	if !voice.Config().WakeWord {
		t.Error("转写会话应启用唤醒门")
	}
	_ = session.Send(ctx, realtimeproto.HaroWakeWordSleep{})
	_ = session.Send(ctx, realtimeproto.HaroWakeWordWake{})
	if got := stream.AwakeCalls(); !slices.Equal(got, []bool{false, true}) {
		t.Errorf("SetAwake 调用 %v，应为 [false true]", got)
	}
	for _, u := range []braintest.Utterance{{Wake: true, Text: "。"}, {}} {
		if done := braintest.Done(t, braintest.Ask(t, session, stream, u)); done.Status != realtimeproto.StatusIncomplete {
			t.Errorf("只有标点的识别结果不回答：%+v", done)
		}
	}
	if model.lastInput.Text != "" {
		t.Error("没有字的识别结果不应调用大模型")
	}
}

// 声纹：识别结果标着「比对过、没认出来」⇒ 不回答；认出来了照常回答。
func TestUnknownSpeakerIsIgnored(t *testing.T) {
	voice := braintest.NewVoice()
	session := newStreamSession(t, voice, &fakeModel{tokens: []string{"好的。"}}, nil)
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{})

	ask := func(speaker string) realtimeproto.ResponseDone {
		return braintest.Done(t, braintest.Ask(t, session, stream,
			braintest.Utterance{Text: "你好", Speaker: speaker, SpeakerChecked: true}))
	}
	if done := ask(""); done.Status != realtimeproto.StatusIncomplete || done.Reason != "unknown_speaker" {
		t.Errorf("陌生人的话不应回答：%+v", done)
	}
	if done := ask("主人"); done.Status != realtimeproto.StatusCompleted {
		t.Errorf("主人的话应照常回答：%+v", done)
	}
}

// 两台终端同时听到唤醒词——只有质量高的那台把唤醒告诉终端，另一台的识别流被送回休眠。
func TestTwoTerminalsHearingWakeWordOnlyOneAnswers(t *testing.T) {
	arbiter := brain.NewWakeArbiter(50 * time.Millisecond)
	open := func() (*brain.Session, *braintest.Stream) {
		voice := braintest.NewVoice()
		session := newStreamSession(t, voice, &fakeModel{}, arbiter)
		return session, braintest.Open(t, session, voice, realtimeproto.SessionUpdate{WakeWord: true})
	}
	near, nearStream := open()
	far, farStream := open()

	_ = farStream.Emit(realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗", Score: 0.30, Level: 0.01})
	_ = nearStream.Emit(realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗", Score: 0.55, Level: 0.04})

	braintest.Expect[realtimeproto.HaroWakeWordDetected](t, near)
	select {
	case event := <-far.Events():
		t.Fatalf("远的那台不该收到任何事件：%v", event)
	case <-time.After(200 * time.Millisecond):
	}
	if got := farStream.AwakeCalls(); !slices.Equal(got, []bool{false}) {
		t.Errorf("远的那台的识别流应被送回休眠：%v", got)
	}
}

// 识别流还没打开（session.update 没带 turn_detection）时收到的音频无处可送：明确拒绝，不在服务端堆积。
func TestAudioBeforeStreamIsRejected(t *testing.T) {
	session := newStreamSession(t, braintest.NewVoice(), &fakeModel{}, nil)
	audio := realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: testFormat, Data: make([]byte, 640)}}
	if err := session.Send(context.Background(), audio); err == nil {
		t.Error("识别流没打开时应拒绝音频")
	}
}

// 识别服务报这句识别失败（带编号）：等这句结果的回答马上以 transcription_failed 结束，不一直等着。
func TestTranscriptionFailureEndsTheTurn(t *testing.T) {
	voice := braintest.NewVoice()
	session := newStreamSession(t, voice, &fakeModel{tokens: []string{"不该调用模型。"}}, nil)
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn})

	braintest.Speak(t, session, stream, braintest.Utterance{})
	braintest.Expect[realtimeproto.ResponseCreated](t, session)
	if err := stream.Emit(realtimeproto.InputAudioTranscriptionFailed{ItemID: stream.LastItemID(),
		Code: realtimeproto.CodeTranscriptionFailed, Message: "识别失败"}); err != nil {
		t.Fatal(err)
	}
	done := braintest.Done(t, braintest.CollectUntilDone(t, session))
	if done.Status != realtimeproto.StatusFailed || done.Reason != realtimeproto.ReasonTranscriptionFailed {
		t.Errorf("应以识别失败结束：%+v", done)
	}
}

// 识别服务既不给结果也不报失败：等到期限就结束这一轮，不一直挂着。
func TestTranscriptionWaitTimesOut(t *testing.T) {
	defer brain.SetTranscriptionWaitTimeoutForTest(50 * time.Millisecond)()
	voice := braintest.NewVoice()
	session := newStreamSession(t, voice, &fakeModel{tokens: []string{"不该调用模型。"}}, nil)
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn})

	braintest.Speak(t, session, stream, braintest.Utterance{})
	braintest.Expect[realtimeproto.ResponseCreated](t, session)
	done := braintest.Done(t, braintest.CollectUntilDone(t, session))
	if done.Status != realtimeproto.StatusFailed || done.Reason != realtimeproto.ReasonTranscriptionFailed {
		t.Errorf("等不到识别结果应结束这一轮：%+v", done)
	}
}

// warnSignal 每收到一条告警级别的日志就发一次信号（只看级别，不比对文字）。
type warnSignal chan struct{}

func (w warnSignal) Enabled(context.Context, slog.Level) bool { return true }
func (w warnSignal) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w warnSignal) WithGroup(string) slog.Handler            { return w }
func (w warnSignal) Handle(_ context.Context, record slog.Record) error {
	if record.Level >= slog.LevelWarn {
		select {
		case w <- struct{}{}:
		default:
		}
	}
	return nil
}

// 识别流真断开时告警；会话正常结束时关掉的流不是断开，不告警（之前每次终端断开都会多一条「识别流断开了」）。
func TestClosingSessionIsNotAStreamDisconnect(t *testing.T) {
	voice := braintest.NewVoice()
	warned := make(warnSignal, 8)
	session := brain.NewSession(brain.Components{Front: brain.Agent{Model: &fakeModel{tokens: []string{"好。"}}}, Synthesizer: fakeSynthesizer{},
		Streaming: voice}, brain.Options{MaxHistoryTurn: 6, StreamReopen: brain.ReopenTiming{Delay: time.Millisecond,
		MaxDelay: 5 * time.Millisecond, LostNoticeAfter: time.Hour}}, slog.New(warned))
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	first := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn})

	_ = first.Close() // 真断开（对端走了）
	select {
	case <-warned:
	case <-time.After(braintest.EventTimeout):
		t.Fatal("识别流真断开时应告警")
	}
	waitOpened(t, voice, 2)

	_ = session.Close()
	select {
	case <-warned:
		t.Error("会话正常结束时关掉的流不应当成断开来告警")
	case <-time.After(100 * time.Millisecond): // 有问题时告警在关流后立刻就打
	}
}
