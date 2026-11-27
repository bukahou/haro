package brain_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/agents"
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

func (f *fakeModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	messages := request.Messages
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
		yield(brain.ReplyDelta{Usage: &brain.ReplyUsage{PromptTokens: 10, OutputTokens: len(f.tokens)}, Stop: brain.StopCompleted}, nil)
	}
}

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
	return newTalkerLogging(t, parts, opts, quiet)
}

// newTalkerLogging 同 newTalker，日志写到 logger。
func newTalkerLogging(t *testing.T, parts brain.Components, opts brain.Options, logger *slog.Logger) *talker {
	t.Helper()
	voice := braintest.NewVoice()
	parts.Streaming = voice
	parts.Front = withTestSteps(parts.Front)
	if parts.NewSegmenter == nil {
		parts.NewSegmenter = func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }
	}
	session := brain.NewSession(parts, opts, logger)
	t.Cleanup(func() { _ = session.Close() })
	return &talker{t: t, session: session, voice: voice,
		stream: braintest.Open(t, session, voice, realtimeproto.SessionUpdate{})}
}

// withTestSteps 给没写步数的代理补上测试用的步数（和前台的定义一样：8 步，最近 4 步里第 3 次算死循环）。
func withTestSteps(agent brain.Agent) brain.Agent {
	if agent.Name == "" {
		agent.Name = "前台"
	}
	if agent.MaxSteps == 0 {
		agent.MaxSteps = 8
	}
	if agent.RepeatLimit == 0 && agent.RepeatWindow == 0 {
		agent.RepeatLimit, agent.RepeatWindow = 3, 4
	}
	return agent
}

// builtinFrontInstructions 是程序里带的前台的基础规则（填好值）。
func builtinFrontInstructions(t *testing.T) string {
	t.Helper()
	catalog, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	front, _ := catalog.Get(agents.Front)
	instructions, err := front.RenderPrompt(brain.PromptValues())
	if err != nil {
		t.Fatal(err)
	}
	return instructions
}

// newTestTalker 是只有模型和语音合成的会话。
func newTestTalker(t *testing.T, model brain.ChatModel, synth brain.SpeechSynthesizer) *talker {
	t.Helper()
	return newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: synth}, brain.Options{MaxHistoryTurn: 6})
}

// ask 让主人说 text，返回这一轮的事件（从识别结果到 response.done）。
func (k *talker) ask(text string) []realtimeproto.ServerEvent {
	k.t.Helper()
	return braintest.Ask(k.t, k.session, k.stream, braintest.Utterance{Text: text})
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
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model, Instructions: "你是测试助手"}, Synthesizer: fakeSynthesizer{}},
		brain.Options{MaxHistoryTurn: 6})

	events := k.ask("你能看到我吗")

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
	if history := withoutSynthetic(model.history); len(history) != 2 || model.history[0].Role != brain.RoleSystem ||
		history[1].Text != "我看到你坐在电脑前面，旁边有一个杯子。还有键盘。" {
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

// 判定说完时服务端已经自己开始回答：这时客户端再请求回答，按「已有进行中的回答」拒绝。
func TestSessionRejectsConcurrentResponse(t *testing.T) {
	k := newTestTalker(t, &fakeModel{tokens: []string{"慢慢说。"}, delay: 200 * time.Millisecond}, fakeSynthesizer{})
	braintest.Speak(t, k.session, k.stream, braintest.Utterance{Text: "你好"})
	braintest.Expect[realtimeproto.ResponseCreated](t, k.session)
	err := k.session.Send(context.Background(), realtimeproto.ResponseCreate{})
	if protocolErr, ok := errors.AsType[*realtimeproto.ProtocolError](err); !ok || protocolErr.Code != realtimeproto.CodeResponseInProgress {
		t.Errorf("进行中的回答未结束时，response.create 应以 response_in_progress 拒绝：%v", err)
	}
}

// create_response 为 false 的客户端（标准 Realtime 用法）：判定说完时不自动回答，等它发 response.create。
func TestSessionWaitsForResponseCreateWhenAsked(t *testing.T) {
	model := braintest.NewModel(t, braintest.Say("好的。"))
	voice := braintest.NewVoice()
	session := brain.NewSession(brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Streaming: voice,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }},
		brain.Options{MaxHistoryTurn: 6}, quiet)
	defer session.Close()
	manual := false
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{
		TurnDetection: &realtimeproto.TurnDetection{Type: "server_vad", CreateResponse: &manual}})

	braintest.Speak(t, session, stream, braintest.Utterance{Text: "你好"})
	select {
	case event := <-session.Events():
		t.Fatalf("没请求回答时不该开始回答，收到 %s", event.Type())
	case <-time.After(100 * time.Millisecond):
	}
	if err := session.Send(context.Background(), realtimeproto.ResponseCreate{}); err != nil {
		t.Fatal(err)
	}
	braintest.Expect[realtimeproto.ResponseCreated](t, session)
	braintest.Recognized(t, stream, braintest.Utterance{Text: "你好"})
	if done := braintest.Done(t, braintest.CollectUntilDone(t, session)); done.Status != realtimeproto.StatusCompleted {
		t.Errorf("请求回答后应正常回答：%+v", done)
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
		{Text: "你问我冰箱里有什么？", Emotion: brain.EmotionTease},
		{Text: "我又不是冰箱。", Emotion: brain.EmotionTease},
		{Text: "哈哈，开玩笑的。", Emotion: brain.EmotionLaugh},
		{Text: "えっ、また寝坊したの？", Emotion: brain.EmotionLaugh}, // 用什么语言念由语音合成自己判断
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
	history := withoutSynthetic(model.history)
	if last := history[len(history)-1].Text; !strings.HasPrefix(last, "[调侃]") {
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
	parts := brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{},
		Conversations: brain.NewConversationBook(nil, opts, quiet)}

	first := newTalker(t, parts, opts)
	first.ask("你好")
	_ = first.session.Close()

	second := newTalker(t, parts, opts)
	second.ask("你好")
	if history := withoutSynthetic(model.history); len(history) != 2 || history[0].Text != "你好" || history[1].Text != "你好呀。" { // 没设系统提示词
		t.Errorf("重连后应接上之前的对话：%+v", model.history)
	}
}

// withoutSynthetic 去掉系统合成的消息（每轮的时间标记、情况说明），只看主人说的话、工具往来和回答。
func withoutSynthetic(messages []brain.ChatMessage) []brain.ChatMessage {
	var kept []brain.ChatMessage
	for _, msg := range messages {
		if !msg.Synthetic && msg.Role != brain.RoleSystem { // 只看对话本身：系统提示词（含工具介绍）另外测
			kept = append(kept, msg)
		}
	}
	return kept
}

// 情绪标记前面的话没有句号：在标记处收尾成一句，用原来的语气；标记之后的才换语气（不并成一句）。
func TestEmotionTagEndsTheSentenceBeforeIt(t *testing.T) {
	model := &fakeModel{tokens: []string{"好的", "[笑]", "哈哈，开玩笑的。"}}
	var seen []brain.Utterance
	k := newTestTalker(t, model, fakeSynthesizer{seen: &seen})
	k.ask("讲个笑话")
	want := []brain.Utterance{{Text: "好的", Emotion: brain.EmotionDefault}, {Text: "哈哈，开玩笑的。", Emotion: brain.EmotionLaugh}}
	if !slices.Equal(seen, want) {
		t.Errorf("got %+v\nwant %+v", seen, want)
	}
}

// attrRecorder 记下告警级别日志的属性（只看级别和属性，不比对文字）。
type attrRecorder struct {
	mu    sync.Mutex
	warns []map[string]string
}

func (r *attrRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *attrRecorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *attrRecorder) WithGroup(string) slog.Handler            { return r }
func (r *attrRecorder) Handle(_ context.Context, record slog.Record) error {
	if record.Level < slog.LevelWarn {
		return nil
	}
	attrs := map[string]string{}
	record.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
	r.mu.Lock()
	r.warns = append(r.warns, attrs)
	r.mu.Unlock()
	return nil
}

func (r *attrRecorder) hasWarn(key, value string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.warns, func(attrs map[string]string) bool { return attrs[key] == value })
}

// 模型在句首写了清单外的情绪标记：照原样念出，记一条带标记的告警（先看实际出现多少再定怎么处理）。
func TestUnknownEmotionTagIsLogged(t *testing.T) {
	logs := &attrRecorder{}
	model := braintest.NewModel(t, braintest.Say("[无奈]好吧，那就这样。"))
	k := newTalkerLogging(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}},
		brain.Options{MaxHistoryTurn: 6}, slog.New(logs))
	if spoken := k.say("算了吧"); !strings.HasPrefix(spoken, "[无奈]") {
		t.Errorf("清单外的标记照原样念：%q", spoken)
	}
	if !logs.hasWarn("tag", "[无奈]") {
		t.Error("应记一条带标记的告警")
	}
}

// 窗口轮数只由对话簿持有：会话的选项和对话簿的不一样时，以对话簿的为准（重启接上时用的也是它）。
func TestHistoryWindowComesFromTheBook(t *testing.T) {
	model := braintest.NewModel(t, braintest.Say("一。"), braintest.Say("二。"), braintest.Say("三。"))
	book := brain.NewConversationBook(nil, brain.Options{MaxHistoryTurn: 1}, quiet)
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Conversations: book},
		brain.Options{MaxHistoryTurn: 6})
	for _, text := range []string{"第一句", "第二句", "第三句"} {
		k.say(text)
	}
	owner := 0
	for _, msg := range model.LastRequest().Messages {
		if msg.Role == brain.RoleUser && !msg.Synthetic {
			owner++
		}
	}
	if owner != 2 { // 窗口里的 1 轮 + 这一句
		t.Errorf("窗口应按对话簿的 1 轮：请求里有 %d 句主人的话", owner)
	}
}

// 模型判断主人要结束对话、调用 end_conversation：这一轮的 response.done 带上「说完就睡」，终端播完就休眠。
// 不用唤醒门的终端没有休眠可言，不提供这个工具。
func TestEndConversationMarksResponseDone(t *testing.T) {
	model := braintest.NewModel(t, braintest.Call("end_conversation", map[string]any{}), braintest.Say("好，我先休息了，有事再叫我。"))
	voice := braintest.NewVoice()
	parts := brain.Components{Front: withTestSteps(brain.Agent{Model: model}), Synthesizer: fakeSynthesizer{}, Streaming: voice,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) }}
	session := brain.NewSession(parts, brain.Options{MaxHistoryTurn: 6}, quiet)
	t.Cleanup(func() { _ = session.Close() })
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{TurnDetection: &turn, WakeWord: true})
	if err := stream.Emit(realtimeproto.HaroWakeWordState{Awake: true}); err != nil {
		t.Fatal(err)
	}
	braintest.Expect[realtimeproto.HaroWakeWordState](t, session)
	done := braintest.Done(t, braintest.Ask(t, session, stream, braintest.Utterance{Text: "好了，你先休息吧"}))
	if done.Status != realtimeproto.StatusCompleted || !done.EndConversation {
		t.Fatalf("应带上说完就睡：%+v", done)
	}
	offered := false
	for _, tool := range model.LastRequest().Tools {
		offered = offered || tool.Name == "end_conversation"
	}
	if !offered {
		t.Error("用唤醒门的终端应提供 end_conversation")
	}

	plain := braintest.NewModel(t, braintest.Say("好的。"))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: plain}, Synthesizer: fakeSynthesizer{}}, brain.Options{MaxHistoryTurn: 6})
	if done := braintest.Done(t, k.ask("你好")); done.EndConversation {
		t.Errorf("没调用就不带：%+v", done)
	}
	for _, tool := range plain.LastRequest().Tools {
		if tool.Name == "end_conversation" {
			t.Error("不用唤醒门时不该提供 end_conversation")
		}
	}
}
