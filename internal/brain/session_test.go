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
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

var testFormat = realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}

type fakeRecognizer struct{ text string }

func (f fakeRecognizer) Recognize(context.Context, realtimeproto.PCMAudio) (string, error) {
	return f.text, nil
}

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

func newTestSession(recognizer brain.SpeechRecognizer, model brain.ChatModel, synth brain.SpeechSynthesizer) *brain.Session {
	return brain.NewSession(brain.Components{
		Recognizer: recognizer, Model: model, Synthesizer: synth,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// askOneTurn 发出一轮完整的客户端事件，收集服务端事件直到 response.done。
func askOneTurn(t *testing.T, ctx context.Context, session *brain.Session, image []byte) []realtimeproto.ServerEvent {
	t.Helper()
	send := func(event realtimeproto.ClientEvent) {
		if err := session.Send(ctx, event); err != nil {
			t.Fatalf("发送 %s: %v", event.Type(), err)
		}
	}
	send(realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: testFormat, Data: make([]byte, 32000)}})
	send(realtimeproto.InputAudioBufferCommit{})
	if image != nil {
		send(realtimeproto.ConversationItemCreate{Item: realtimeproto.ConversationItem{Role: "user",
			Content: []realtimeproto.ContentPart{{Type: "input_image", ImageJPEG: image}}}})
	}
	send(realtimeproto.ResponseCreate{})
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
			t.Fatalf("等 response.done 超时，已收到 %v", events)
		}
	}
}

func eventTypes(events []realtimeproto.ServerEvent) []string {
	var types []string
	for _, event := range events {
		types = append(types, event.Type())
	}
	return types
}

func TestSessionStreamsSentencesInOrder(t *testing.T) {
	model := &fakeModel{tokens: []string{"我看到你", "坐在电脑前面，", "旁边有", "一个杯子。", "还有键盘。"}, delay: 5 * time.Millisecond}
	session := newTestSession(fakeRecognizer{text: "你能看到我吗"}, model, fakeSynthesizer{})
	defer session.Close()
	ctx := context.Background()
	_ = session.Send(ctx, realtimeproto.SessionUpdate{Instructions: "你是测试助手"})

	events := askOneTurn(t, ctx, session, []byte("jpeg"))

	wantTypes := []string{
		"conversation.item.input_audio_transcription.completed",
		"response.output_audio_transcript.delta", "response.output_audio.delta",
		"response.output_audio_transcript.delta", "response.output_audio.delta",
		"response.output_audio_transcript.delta", "response.output_audio.delta",
		"response.done",
	}
	if got := eventTypes(events); !slices.Equal(got, wantTypes) {
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
	done := events[len(events)-1].(realtimeproto.ResponseDone)
	if done.Status != realtimeproto.StatusCompleted || done.Usage == nil || done.Usage.InputTokens != 10 {
		t.Errorf("response.done = %+v", done)
	}
	// D-B ②：画面不直接附在用户的话上，模型要看时调用 look_at_camera
	if model.lastInput.Image != nil || model.lastInput.Text != "你能看到我吗" {
		t.Errorf("模型收到的输入 = %+v", model.lastInput)
	}

	// 第二轮：历史里有系统提示词 + 上一轮问答，且不带图片
	askOneTurn(t, ctx, session, nil)
	if len(model.history) != 3 || model.history[0].Role != brain.RoleSystem ||
		model.history[2].Text != "我看到你坐在电脑前面，旁边有一个杯子。还有键盘。" {
		t.Errorf("第二轮历史 = %+v", model.history)
	}
	if model.lastInput.Image != nil {
		t.Error("第二轮不应带上一轮的图片")
	}
}

func TestSessionEmptyTranscript(t *testing.T) {
	session := newTestSession(fakeRecognizer{text: "  "}, &fakeModel{}, fakeSynthesizer{})
	defer session.Close()
	events := askOneTurn(t, context.Background(), session, nil)
	done := events[len(events)-1].(realtimeproto.ResponseDone)
	if done.Status != realtimeproto.StatusIncomplete || done.Reason != "empty_transcript" {
		t.Errorf("response.done = %+v", done)
	}
}

func TestSessionSynthesisFailure(t *testing.T) {
	model := &fakeModel{tokens: []string{"第一句话。", "第二句话。"}}
	session := newTestSession(fakeRecognizer{text: "你好"}, model, fakeSynthesizer{failOn: "第二句话。"})
	defer session.Close()
	events := askOneTurn(t, context.Background(), session, nil)
	types := eventTypes(events)
	if !slices.Contains(types, "error") {
		t.Errorf("应该有 error 事件：%v", types)
	}
	done := events[len(events)-1].(realtimeproto.ResponseDone)
	if done.Status != realtimeproto.StatusFailed {
		t.Errorf("response.done = %+v", done)
	}
}

func TestSessionCancel(t *testing.T) {
	model := &fakeModel{tokens: []string{"很", "长", "的", "回", "答"}, delay: time.Second}
	session := newTestSession(fakeRecognizer{text: "你好"}, model, fakeSynthesizer{})
	defer session.Close()
	ctx := context.Background()
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = session.Send(ctx, realtimeproto.ResponseCancel{})
	}()
	started := time.Now()
	events := askOneTurn(t, ctx, session, nil)
	done := events[len(events)-1].(realtimeproto.ResponseDone)
	if done.Status != realtimeproto.StatusCancelled {
		t.Errorf("response.done = %+v", done)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Errorf("取消用了 %s，应立即生效", elapsed)
	}
}

func TestSessionRejectsConcurrentResponse(t *testing.T) {
	model := &fakeModel{tokens: []string{"慢慢说。"}, delay: 200 * time.Millisecond}
	session := newTestSession(fakeRecognizer{text: "你好"}, model, fakeSynthesizer{})
	defer session.Close()
	ctx := context.Background()
	_ = session.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: testFormat, Data: []byte{0, 0}}})
	_ = session.Send(ctx, realtimeproto.InputAudioBufferCommit{})
	if err := session.Send(ctx, realtimeproto.ResponseCreate{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Send(ctx, realtimeproto.ResponseCreate{}); err == nil {
		t.Error("进行中的回答未结束时，第二个 response.create 应被拒绝")
	}
}

// 情绪标记：取出来交给语音合成，标记管到下一个标记为止；终端看到的文字不带标记；历史保留模型原话。
func TestSessionEmotionTags(t *testing.T) {
	model := &fakeModel{tokens: []string{"[调侃]你问我", "冰箱里有什么？", "我又不是冰箱。", "[笑]", "哈哈，开玩笑的。",
		"えっ、また寝坊したの？"}}
	var seen []brain.Utterance
	session := newTestSession(fakeRecognizer{text: "冰箱里有什么"}, model, fakeSynthesizer{seen: &seen})
	defer session.Close()
	ctx := context.Background()
	events := askOneTurn(t, ctx, session, nil)

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
	askOneTurn(t, ctx, session, nil)
	if last := model.history[len(model.history)-1].Text; !strings.HasPrefix(last, "[调侃]") {
		t.Errorf("历史里应保留模型原话（含标记），得到 %q", last)
	}
}

// v4 D3：终端说没有音箱 ⇒ 只发文字、不调语音合成；音箱回来后恢复合成。
func TestSessionSkipsSynthesisWithoutSpeaker(t *testing.T) {
	var seen []brain.Utterance
	session := newTestSession(fakeRecognizer{text: "你好"}, &fakeModel{tokens: []string{"你好呀。"}}, fakeSynthesizer{seen: &seen})
	defer session.Close()
	ctx := context.Background()
	_ = session.Send(ctx, realtimeproto.HaroTerminalDevices{Microphone: "USB 麦克风"})

	events := askOneTurn(t, ctx, session, nil)
	want := []string{"conversation.item.input_audio_transcription.completed",
		"response.output_audio_transcript.delta", "response.done"}
	if got := eventTypes(events); !slices.Equal(got, want) || len(seen) != 0 {
		t.Fatalf("没有音箱时应只发文字：%v，合成了 %d 句", got, len(seen))
	}
	if done := events[len(events)-1].(realtimeproto.ResponseDone); done.Status != realtimeproto.StatusCompleted {
		t.Errorf("只发文字也算完成：%+v", done)
	}

	_ = session.Send(ctx, realtimeproto.HaroTerminalDevices{Microphone: "USB 麦克风", Speaker: "音箱"})
	askOneTurn(t, ctx, session, nil)
	if len(seen) != 1 {
		t.Errorf("音箱回来后应恢复合成：%d", len(seen))
	}
}

// v4 D1：对话跟人走，不跟连接走——断开重连（新的会话）后，模型看得到上一条连接里的对话。
func TestConversationSurvivesReconnect(t *testing.T) {
	model := &fakeModel{tokens: []string{"你好呀。"}}
	parts := brain.Components{
		Recognizer: fakeRecognizer{text: "你好"}, Model: model, Synthesizer: fakeSynthesizer{},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := brain.Options{MaxHistoryTurn: 6}
	parts.Conversations = brain.NewConversationBook(nil, opts, logger)
	ctx := context.Background()

	first := brain.NewSession(parts, opts, logger)
	askOneTurn(t, ctx, first, nil)
	_ = first.Close()

	second := brain.NewSession(parts, opts, logger)
	defer second.Close()
	askOneTurn(t, ctx, second, nil)
	if len(model.history) != 2 || model.history[0].Text != "你好" || model.history[1].Text != "你好呀。" { // 没设系统提示词
		t.Errorf("重连后应接上之前的对话：%+v", model.history)
	}
}
