package brain_test

import (
	"context"
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
	parts := brain.Components{Model: model, Synthesizer: fakeSynthesizer{}, WakeArbiter: arbiter,
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

	// 连接断开：等识别结果的回答应失败，而不是卡住
	braintest.Speak(t, session, stream, braintest.Utterance{})
	braintest.Expect[realtimeproto.ResponseCreated](t, session)
	_ = stream.Close()
	events = braintest.CollectUntilDone(t, session)
	if done := braintest.Done(t, events); done.Status != realtimeproto.StatusFailed {
		t.Errorf("断开后 response.done = %+v", done)
	}
	if !slices.Contains(braintest.Types(events), "error") {
		t.Errorf("断开后应有 error 事件：%v", braintest.Types(events))
	}
}

func TestServerVADRejectedWithoutStreaming(t *testing.T) {
	session := newStreamSession(t, nil, &fakeModel{}, nil)
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	if err := session.Send(context.Background(), realtimeproto.SessionUpdate{TurnDetection: &turn}); err == nil {
		t.Error("没有配置流式识别时应拒绝 server_vad")
	}
}

func TestSessionWakeWordAcknowledgement(t *testing.T) {
	voice := braintest.NewVoice()
	model := &fakeModel{tokens: []string{"不应该调用模型。"}}
	session := newStreamSession(t, voice, model, nil)
	ctx := context.Background()
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn, WakeWord: true})
	if !voice.Config().WakeWord {
		t.Error("转写会话应启用唤醒门")
	}
	// 唤醒门控制转发给转写会话
	_ = session.Send(ctx, realtimeproto.HaroWakeWordSleep{})
	_ = session.Send(ctx, realtimeproto.HaroWakeWordWake{})
	if got := stream.AwakeCalls(); !slices.Equal(got, []bool{false, true}) {
		t.Errorf("SetAwake 调用 %v，应为 [false true]", got)
	}

	// 只叫了唤醒词：识别结果只有标点 → 回「我在。」，不调用大模型
	events := braintest.Ask(t, session, stream, braintest.Utterance{Wake: true, Text: "。"})
	if done := braintest.Done(t, events); done.Status != realtimeproto.StatusCompleted || braintest.Spoken(events) != "我在。" {
		t.Errorf("应回「我在。」：播出 %q，done %+v", braintest.Spoken(events), done)
	}
	if model.lastInput.Text != "" {
		t.Error("只叫唤醒词时不应调用大模型")
	}

	// 之后没叫唤醒词的空识别：照常是 incomplete
	if done := braintest.Done(t, braintest.Ask(t, session, stream, braintest.Utterance{})); done.Status != realtimeproto.StatusIncomplete {
		t.Errorf("未唤醒的空识别应为 incomplete：%+v", done)
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
