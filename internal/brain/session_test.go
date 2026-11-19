package brain_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

var (
	testFormat = realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}
	quiet      = slog.New(slog.NewTextHandler(io.Discard, nil))
)

// fakeModel 按 tokens 逐个吐字，每个间隔 delay；记录收到的最后一次输入。
type fakeModel struct {
	tokens    []string
	delay     time.Duration
	lastInput brain.ChatMessage
	history   []brain.ChatMessage
}

func (f *fakeModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, _ []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	f.lastInput, f.history = messages[len(messages)-1], messages[:len(messages)-1]
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, token := range f.tokens {
			select {
			case <-time.After(f.delay):
			case <-ctx.Done():
				yield(brain.ReplyDelta{}, ctx.Err())
				return
			}
			if !yield(brain.ReplyDelta{Text: token}, nil) {
				return
			}
		}
		yield(brain.ReplyDelta{Usage: &brain.ReplyUsage{PromptTokens: 10, OutputTokens: len(f.tokens)}}, nil)
	}
}

func (f *fakeModel) Warmup(context.Context) error { return nil }

// fakeSynthesizer 把句子原样放进 PCM，方便断言顺序；failOn 非空时对该句报错；seen 非空时记下收到的每一句。
type fakeSynthesizer struct {
	failOn string
	seen   *[]brain.Utterance
}

func (f fakeSynthesizer) Synthesize(_ context.Context, utterance brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		if f.seen != nil {
			*f.seen = append(*f.seen, utterance)
		}
		if utterance.Text == f.failOn {
			yield(realtimeproto.PCMAudio{}, errors.New("合成失败"))
			return
		}
		yield(realtimeproto.PCMAudio{Format: testFormat, Data: []byte(utterance.Text)}, nil)
	}
}

// talker 是一个会话加上驱动它的识别流：测试里「主人说一句话」就是 ask。
type talker struct {
	t       *testing.T
	session *brain.Session
	voice   *braintest.Voice
	stream  *braintest.Stream
}

// newTalker 建会话并打开识别流（服务端判断说完，不开唤醒门）。parts 里没给断句器时用 4 / 40 字的切句器。
func newTalker(t *testing.T, parts brain.Components, opts brain.Options) *talker {
	t.Helper()
	voice := braintest.NewVoice()
	parts.Streaming = voice
	if parts.NewSegmenter == nil {
		parts.NewSegmenter = func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }
	}
	session := brain.NewSession(parts, opts, quiet)
	t.Cleanup(func() { _ = session.Close() })
	return &talker{t: t, session: session, voice: voice,
		stream: braintest.Open(t, session, voice, realtimeproto.SessionUpdate{})}
}

// newTestTalker 是只有模型和语音合成的会话。
func newTestTalker(t *testing.T, model brain.ChatModel, synth brain.SpeechSynthesizer) *talker {
	t.Helper()
	return newTalker(t, brain.Components{Model: model, Synthesizer: synth}, brain.Options{MaxHistoryTurn: 6})
}

// ask 让主人说 text，返回这一轮的事件（从识别结果到 response.done）。
func (k *talker) ask(text string) []realtimeproto.ServerEvent {
	k.t.Helper()
	return braintest.Ask(k.t, k.session, k.stream, braintest.Utterance{Text: text})
}

// askWithImage 同 ask，终端随这句话带上画面。
func (k *talker) askWithImage(text string, image []byte) []realtimeproto.ServerEvent {
	k.t.Helper()
	return braintest.Ask(k.t, k.session, k.stream, braintest.Utterance{Text: text, Image: image})
}

// say 让主人说 text，要求回答正常完成，返回播出的文字。
func (k *talker) say(text string) string {
	k.t.Helper()
	events := k.ask(text)
	if done := braintest.Done(k.t, events); done.Status != realtimeproto.StatusCompleted {
		k.t.Fatalf("「%s」的回答没有完成：%+v", text, done)
	}
	return braintest.Spoken(events)
}

// send 发一个客户端事件，出错就终止测试。
func (k *talker) send(event realtimeproto.ClientEvent) {
	k.t.Helper()
	if err := k.session.Send(context.Background(), event); err != nil {
		k.t.Fatalf("发送 %s：%v", event.Type(), err)
	}
}

func TestSessionStreamsSentencesInOrder(t *testing.T) {
	model := &fakeModel{tokens: []string{"我看到你", "坐在电脑前面，", "旁边有", "一个杯子。", "还有键盘。"}, delay: 5 * time.Millisecond}
	k := newTestTalker(t, model, fakeSynthesizer{})
	k.send(realtimeproto.SessionUpdate{Instructions: "你是测试助手"})

	events := k.askWithImage("你能看到我吗", []byte("jpeg"))

	wantTypes := []string{
		"conversation.item.input_audio_transcription.completed",
		"response.output_audio_transcript.delta", "response.output_audio.delta",
		"response.output_audio_transcript.delta", "response.output_audio.delta",
		"response.output_audio_transcript.delta", "response.output_audio.delta",
		"response.done",
	}
	if got := braintest.Types(events); !slices.Equal(got, wantTypes) {
		t.Fatalf("事件顺序\n got %v\nwant %v", got, wantTypes)
	}
	var spoken []string
	for _, event := range events {
		if audio, ok := event.(realtimeproto.ResponseOutputAudioDelta); ok {
			spoken = append(spoken, string(audio.Audio.Data))
		}
	}
	wantSpoken := []string{"我看到你坐在电脑前面，", "旁边有一个杯子。", "还有键盘。"}
	if !slices.Equal(spoken, wantSpoken) {
		t.Errorf("播出顺序 got %q, want %q", spoken, wantSpoken)
	}
	done := braintest.Done(t, events)
	if done.Status != realtimeproto.StatusCompleted || done.Usage == nil || done.Usage.InputTokens != 10 {
		t.Errorf("response.done = %+v", done)
	}
	// 画面不直接附在用户的话上，模型要看时调用 look_at_camera
	if model.lastInput.Image != nil || model.lastInput.Text != "你能看到我吗" {
		t.Errorf("模型收到的输入 = %+v", model.lastInput)
	}

	// 第二轮：历史里有系统提示词 + 上一轮问答，且不带图片
	k.ask("你能看到我吗")
	if len(model.history) != 3 || model.history[0].Role != brain.RoleSystem ||
		model.history[2].Text != "我看到你坐在电脑前面，旁边有一个杯子。还有键盘。" {
		t.Errorf("第二轮历史 = %+v", model.history)
	}
	if model.lastInput.Image != nil {
		t.Error("第二轮不应带上一轮的图片")
	}
}

func TestSessionEmptyTranscript(t *testing.T) {
	k := newTestTalker(t, &fakeModel{}, fakeSynthesizer{})
	done := braintest.Done(t, k.ask("  "))
	if done.Status != realtimeproto.StatusIncomplete || done.Reason != "empty_transcript" {
		t.Errorf("response.done = %+v", done)
	}
}

func TestSessionSynthesisFailure(t *testing.T) {
	model := &fakeModel{tokens: []string{"第一句话。", "第二句话。"}}
	k := newTestTalker(t, model, fakeSynthesizer{failOn: "第二句话。"})
	events := k.ask("你好")
	if !slices.Contains(braintest.Types(events), "error") {
		t.Errorf("应该有 error 事件：%v", braintest.Types(events))
	}
	if done := braintest.Done(t, events); done.Status != realtimeproto.StatusFailed {
		t.Errorf("response.done = %+v", done)
	}
}

func TestSessionCancel(t *testing.T) {
	model := &fakeModel{tokens: []string{"很", "长", "的", "回", "答"}, delay: time.Second}
	k := newTestTalker(t, model, fakeSynthesizer{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = k.session.Send(context.Background(), realtimeproto.ResponseCancel{})
	}()
	started := time.Now()
	if done := braintest.Done(t, k.ask("你好")); done.Status != realtimeproto.StatusCancelled {
		t.Errorf("response.done = %+v", done)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Errorf("取消用了 %s，应立即生效", elapsed)
	}
}

func TestSessionRejectsConcurrentResponse(t *testing.T) {
	k := newTestTalker(t, &fakeModel{tokens: []string{"慢慢说。"}, delay: 200 * time.Millisecond}, fakeSynthesizer{})
	braintest.Speak(t, k.session, k.stream, braintest.Utterance{Text: "你好"})
	k.send(realtimeproto.ResponseCreate{})
	if err := k.session.Send(context.Background(), realtimeproto.ResponseCreate{}); err == nil {
		t.Error("进行中的回答未结束时，第二个 response.create 应被拒绝")
	}
}

// 情绪标记：取出来交给语音合成，标记管到下一个标记为止；终端看到的文字不带标记；历史保留模型原话。
func TestSessionEmotionTags(t *testing.T) {
	model := &fakeModel{tokens: []string{"[调侃]你问我", "冰箱里有什么？", "我又不是冰箱。", "[笑]", "哈哈，开玩笑的。",
		"えっ、また寝坊したの？"}}
	var seen []brain.Utterance
	k := newTestTalker(t, model, fakeSynthesizer{seen: &seen})
	events := k.ask("冰箱里有什么")

	want := []brain.Utterance{
		{Text: "你问我冰箱里有什么？", Emotion: brain.EmotionTease, Language: brain.LanguageChinese},
		{Text: "我又不是冰箱。", Emotion: brain.EmotionTease, Language: brain.LanguageChinese},
		{Text: "哈哈，开玩笑的。", Emotion: brain.EmotionLaugh, Language: brain.LanguageChinese},
		{Text: "えっ、また寝坊したの？", Emotion: brain.EmotionLaugh, Language: brain.LanguageJapanese},
	}
	if !slices.Equal(seen, want) {
		t.Errorf("交给语音合成的句子\n got %+v\nwant %+v", seen, want)
	}
	for _, event := range events {
		if delta, ok := event.(realtimeproto.ResponseOutputAudioTranscriptDelta); ok && strings.ContainsAny(delta.Delta, "[]") {
			t.Errorf("终端看到的文字不应带标记：%q", delta.Delta)
		}
	}
	k.ask("冰箱里有什么")
	if last := model.history[len(model.history)-1].Text; !strings.HasPrefix(last, "[调侃]") {
		t.Errorf("历史里应保留模型原话（含标记），得到 %q", last)
	}
}

// 终端说没有音箱 ⇒ 只发文字、不调语音合成；音箱回来后恢复合成。
func TestSessionSkipsSynthesisWithoutSpeaker(t *testing.T) {
	var seen []brain.Utterance
	k := newTestTalker(t, &fakeModel{tokens: []string{"你好呀。"}}, fakeSynthesizer{seen: &seen})
	k.send(realtimeproto.HaroTerminalDevices{Microphone: "USB 麦克风"})

	events := k.ask("你好")
	want := []string{"conversation.item.input_audio_transcription.completed",
		"response.output_audio_transcript.delta", "response.done"}
	if got := braintest.Types(events); !slices.Equal(got, want) || len(seen) != 0 {
		t.Fatalf("没有音箱时应只发文字：%v，合成了 %d 句", got, len(seen))
	}
	if done := braintest.Done(t, events); done.Status != realtimeproto.StatusCompleted {
		t.Errorf("只发文字也算完成：%+v", done)
	}

	k.send(realtimeproto.HaroTerminalDevices{Microphone: "USB 麦克风", Speaker: "音箱"})
	k.ask("你好")
	if len(seen) != 1 {
		t.Errorf("音箱回来后应恢复合成：%d", len(seen))
	}
}

// 对话跟人走，不跟连接走——断开重连（新的会话）后，模型看得到上一条连接里的对话。
func TestConversationSurvivesReconnect(t *testing.T) {
	model := &fakeModel{tokens: []string{"你好呀。"}}
	opts := brain.Options{MaxHistoryTurn: 6}
	parts := brain.Components{Model: model, Synthesizer: fakeSynthesizer{},
		Conversations: brain.NewConversationBook(nil, opts, quiet)}

	first := newTalker(t, parts, opts)
	first.ask("你好")
	_ = first.session.Close()

	second := newTalker(t, parts, opts)
	second.ask("你好")
	if len(model.history) != 2 || model.history[0].Text != "你好" || model.history[1].Text != "你好呀。" { // 没设系统提示词
		t.Errorf("重连后应接上之前的对话：%+v", model.history)
	}
}
