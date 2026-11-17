package brain_test

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

// scriptedToolModel：消息列表最后一条是用户的话时，按 script 返回工具调用；最后一条是工具结果时，把结果念出来。
type scriptedToolModel struct {
	mu        sync.Mutex
	script    map[string]brain.ToolCall // 用户的话 → 要调用的工具
	sawTools  []int                     // 每次调用时给了几个工具
	lastInput []brain.ChatMessage
}

func (m *scriptedToolModel) StreamReply(_ context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.mu.Lock()
	m.sawTools = append(m.sawTools, len(tools))
	m.lastInput = messages
	m.mu.Unlock()
	last := messages[len(messages)-1]
	return func(yield func(brain.ReplyDelta, error) bool) {
		switch {
		case last.Role == brain.RoleTool:
			yield(brain.ReplyDelta{Text: "查到了：" + last.Text + "。"}, nil)
		case len(tools) > 0 && m.script[last.Text].Name != "":
			yield(brain.ReplyDelta{ToolCalls: []brain.ToolCall{m.script[last.Text]}}, nil)
		default:
			yield(brain.ReplyDelta{Text: "好的。"}, nil)
		}
	}
}
func (m *scriptedToolModel) Warmup(context.Context) error { return nil }

// fakeToolbox 记录调用；delete_note 需要确认。
type fakeToolbox struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeToolbox) Specs() []brain.ToolSpec {
	return []brain.ToolSpec{
		{Name: "search_notes", Parameters: json.RawMessage(`{}`)},
		{Name: "delete_note", Parameters: json.RawMessage(`{}`), RequiresConfirmation: true},
	}
}
func (f *fakeToolbox) Call(_ context.Context, call brain.ToolCall) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call.Name)
	if call.Name == "search_notes" {
		return "车停在C3的7号位", nil
	}
	return `{"deleted":true}`, nil
}
func (f *fakeToolbox) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// askText 用按键模式的事件跑一轮，识别结果固定为 text，返回播出的文字。
func askText(t *testing.T, session *brain.Session, recognizer *switchableRecognizer, text string) string {
	t.Helper()
	recognizer.set(text)
	events := askOneTurn(t, context.Background(), session, nil)
	var spoken strings.Builder
	for _, event := range events {
		if audio, ok := event.(realtimeproto.ResponseOutputAudioDelta); ok {
			spoken.Write(audio.Audio.Data)
		}
	}
	if done := events[len(events)-1].(realtimeproto.ResponseDone); done.Status != realtimeproto.StatusCompleted {
		t.Fatalf("「%s」的回答没有完成：%+v", text, done)
	}
	return spoken.String()
}

type switchableRecognizer struct {
	mu   sync.Mutex
	text string
}

func (r *switchableRecognizer) set(text string) { r.mu.Lock(); r.text = text; r.mu.Unlock() }
func (r *switchableRecognizer) Recognize(context.Context, realtimeproto.PCMAudio) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.text, nil
}

func newToolSession(model brain.ChatModel, toolbox brain.Toolbox, recognizer brain.SpeechRecognizer) *brain.Session {
	return brain.NewSession(brain.Components{
		Recognizer: recognizer, Model: model, Synthesizer: fakeSynthesizer{}, Tools: toolbox,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestToolLoopReadOnlyTool(t *testing.T) {
	model := &scriptedToolModel{script: map[string]brain.ToolCall{
		"我的车停在哪了？": {Name: "search_notes", Arguments: map[string]any{"query": "车停在哪"}}}}
	toolbox := &fakeToolbox{}
	recognizer := &switchableRecognizer{}
	session := newToolSession(model, toolbox, recognizer)
	defer session.Close()

	if spoken := askText(t, session, recognizer, "我的车停在哪了？"); spoken != "查到了：车停在C3的7号位。" {
		t.Errorf("播出 %q", spoken)
	}
	if got := toolbox.called(); len(got) != 1 || got[0] != "search_notes" {
		t.Errorf("工具调用 %v", got)
	}
	// 第二轮把工具结果交回模型：消息里应有 assistant(tool_calls) + tool(result)
	model.mu.Lock()
	defer model.mu.Unlock()
	n := len(model.lastInput)
	if n < 2 || model.lastInput[n-2].Role != brain.RoleAssistant || len(model.lastInput[n-2].ToolCalls) != 1 ||
		model.lastInput[n-1].Role != brain.RoleTool || model.lastInput[n-1].ToolName != "search_notes" {
		t.Errorf("工具往来没有正确交回模型：%+v", model.lastInput[max(n-2, 0):])
	}
}

// meteredModel 在每次请求的最后报告用量（输入 100、输出 5）。
type meteredModel struct{ *scriptedToolModel }

func (m meteredModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for delta, err := range m.scriptedToolModel.StreamReply(ctx, messages, tools) {
			if !yield(delta, err) {
				return
			}
		}
		yield(brain.ReplyDelta{Usage: &brain.ReplyUsage{PromptTokens: 100, OutputTokens: 5}}, nil)
	}
}

// 调工具时一次回答请求了模型两次：用量要两次加起来（v3 U-1），不能只算最后一次。
func TestToolLoopUsageAddsUpAllRequests(t *testing.T) {
	model := meteredModel{&scriptedToolModel{script: map[string]brain.ToolCall{
		"我的车停在哪了？": {Name: "search_notes", Arguments: map[string]any{"query": "车停在哪"}}}}}
	recognizer := &switchableRecognizer{}
	session := newToolSession(model, &fakeToolbox{}, recognizer)
	defer session.Close()

	recognizer.set("我的车停在哪了？")
	events := askOneTurn(t, context.Background(), session, nil)
	done := events[len(events)-1].(realtimeproto.ResponseDone)
	if done.Usage == nil || done.Usage.InputTokens != 200 || done.Usage.OutputTokens != 10 {
		t.Errorf("用量应为两次请求之和（输入 200、输出 10）：%+v", done.Usage)
	}
}

func TestToolLoopAsksConfirmationBeforeDestructiveTool(t *testing.T) {
	model := &scriptedToolModel{script: map[string]brain.ToolCall{
		"把那条删了": {Name: "delete_note", Arguments: map[string]any{"query": "车停车位"}}}}
	toolbox := &fakeToolbox{}
	recognizer := &switchableRecognizer{}
	session := newToolSession(model, toolbox, recognizer)
	defer session.Close()

	if spoken := askText(t, session, recognizer, "把那条删了"); !strings.Contains(spoken, "确定要删掉和「车停车位」有关的那条吗") {
		t.Fatalf("应先问确认，播出 %q", spoken)
	}
	if len(toolbox.called()) != 0 {
		t.Fatal("确认之前不应执行删除")
	}
	// 确认后由模型根据执行结果回答（v3 D-1）：替身模型会把工具结果念出来
	if spoken := askText(t, session, recognizer, "确认"); !strings.Contains(spoken, `{"deleted":true}`) {
		t.Errorf("确认后应根据删除结果回答，播出 %q", spoken)
	}
	if got := toolbox.called(); len(got) != 1 || got[0] != "delete_note" {
		t.Errorf("确认后应执行删除：%v", got)
	}

	// 再问一次、这次说「算了」→ 不执行
	askText(t, session, recognizer, "把那条删了")
	if spoken := askText(t, session, recognizer, "算了"); spoken != "好的，不做了。" {
		t.Errorf("取消后播出 %q", spoken)
	}
	// 再问一次、这次说别的 → 放弃删除，照常回答
	askText(t, session, recognizer, "把那条删了")
	if spoken := askText(t, session, recognizer, "今天天气怎么样？"); spoken != "好的。" {
		t.Errorf("说别的应照常回答，播出 %q", spoken)
	}
	if got := toolbox.called(); len(got) != 1 {
		t.Errorf("取消和说别的都不应执行删除：%v", got)
	}
}

func TestToolLoopStopsAfterMaxRounds(t *testing.T) {
	// 模型每一轮都要求调用工具：第 4 轮应不再提供工具，逼它直接回答
	model := &loopingModel{}
	recognizer := &switchableRecognizer{}
	session := newToolSession(model, &fakeToolbox{}, recognizer)
	defer session.Close()
	askText(t, session, recognizer, "查一下")
	if got := model.rounds(); len(got) != 4 || got[3] != 0 {
		t.Errorf("每轮提供的工具数 %v，应为 3 轮有工具 + 最后 1 轮没有", got)
	}
}

type loopingModel struct {
	mu       sync.Mutex
	sawTools []int
}

func (m *loopingModel) StreamReply(_ context.Context, _ []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.mu.Lock()
	m.sawTools = append(m.sawTools, len(tools))
	m.mu.Unlock()
	return func(yield func(brain.ReplyDelta, error) bool) {
		if len(tools) > 0 {
			yield(brain.ReplyDelta{ToolCalls: []brain.ToolCall{{Name: "search_notes"}}}, nil)
			return
		}
		yield(brain.ReplyDelta{Text: "没查到。"}, nil)
	}
}
func (m *loopingModel) Warmup(context.Context) error { return nil }
func (m *loopingModel) rounds() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.sawTools...)
}

func TestToolExchangeIsKeptInHistory(t *testing.T) {
	model := &scriptedToolModel{script: map[string]brain.ToolCall{
		"我的车停在哪了？": {Name: "search_notes", Arguments: map[string]any{"query": "车停在哪"}}}}
	recognizer := &switchableRecognizer{}
	session := newToolSession(model, &fakeToolbox{}, recognizer)
	defer session.Close()
	askText(t, session, recognizer, "我的车停在哪了？")
	askText(t, session, recognizer, "谢谢")

	model.mu.Lock()
	defer model.mu.Unlock()
	var roles []brain.ChatRole
	for _, msg := range model.lastInput {
		roles = append(roles, msg.Role)
	}
	// user(车停在哪), assistant(tool_calls), tool(结果), assistant(回答), user(谢谢)（这个测试会话没有设系统提示词）
	want := []brain.ChatRole{brain.RoleUser, brain.RoleAssistant, brain.RoleTool, brain.RoleAssistant, brain.RoleUser}
	if len(roles) != len(want) {
		t.Fatalf("第二轮的消息角色 %v，应为 %v（上一轮的工具往来要留在历史里）", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("第二轮的消息角色 %v，应为 %v", roles, want)
		}
	}
}

// cameraModel：用户问「我手里拿着什么」时调用 look_at_camera；收到画面后说出画面内容。
type cameraModel struct {
	mu   sync.Mutex
	seen [][]brain.ChatMessage
}

func (m *cameraModel) StreamReply(_ context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.mu.Lock()
	m.seen = append(m.seen, messages)
	m.mu.Unlock()
	last := messages[len(messages)-1]
	return func(yield func(brain.ReplyDelta, error) bool) {
		switch {
		case len(last.Image) > 0:
			yield(brain.ReplyDelta{Text: "你拿着" + string(last.Image) + "。"}, nil)
		case last.Text == "我手里拿着什么？" && len(tools) > 0:
			yield(brain.ReplyDelta{ToolCalls: []brain.ToolCall{{Name: "look_at_camera"}}}, nil)
		default:
			yield(brain.ReplyDelta{Text: "哈哈。"}, nil)
		}
	}
}
func (m *cameraModel) Warmup(context.Context) error { return nil }

func TestLookAtCameraOnDemand(t *testing.T) {
	model := &cameraModel{}
	recognizer := &switchableRecognizer{}
	session := newToolSession(model, nil, recognizer) // 没有工具箱也能看画面
	defer session.Close()

	recognizer.set("我手里拿着什么？")
	events := askOneTurn(t, context.Background(), session, []byte("手机"))
	var spoken strings.Builder
	for _, event := range events {
		if audio, ok := event.(realtimeproto.ResponseOutputAudioDelta); ok {
			spoken.Write(audio.Audio.Data)
		}
	}
	if spoken.String() != "你拿着手机。" {
		t.Fatalf("应调用 look_at_camera 后根据画面回答，播出 %q", spoken.String())
	}

	recognizer.set("讲个笑话吧")
	askOneTurn(t, context.Background(), session, []byte("手机"))
	model.mu.Lock()
	defer model.mu.Unlock()
	last := model.seen[len(model.seen)-1]
	for _, msg := range last {
		if len(msg.Image) > 0 {
			t.Fatalf("闲聊时不应带画面，历史里也不应留图片：%+v", msg)
		}
	}
	// 历史里应留下「当时看了画面」的替身，而不是图片
	found := false
	for _, msg := range last {
		if strings.Contains(msg.Text, "当时看了摄像头画面") {
			found = true
		}
	}
	if !found {
		t.Error("历史里应留下「当时看了摄像头画面」")
	}
}

// 一次删多条：确认问句把每一条都列出来（v3 D-1）。
func TestConfirmationListsEveryTarget(t *testing.T) {
	model := &scriptedToolModel{script: map[string]brain.ToolCall{
		"把取快递和取干洗删了": {Name: "delete_note", Arguments: map[string]any{"queries": []any{"明天下午去取快递", "周三取干洗"}}}}}
	recognizer := &switchableRecognizer{}
	session := newToolSession(model, &fakeToolbox{}, recognizer)
	defer session.Close()
	if spoken := askText(t, session, recognizer, "把取快递和取干洗删了"); !strings.Contains(spoken, "「明天下午去取快递」「周三取干洗」有关的这几条吗") {
		t.Errorf("应列出每一条，播出 %q", spoken)
	}
}
