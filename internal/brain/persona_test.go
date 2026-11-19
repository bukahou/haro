package brain_test

import (
	"context"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
)

var testPersonas = []brain.Persona{
	{ID: "buddy", Name: "毒舌搭子", Aliases: []string{"毒舌", "默认"}, Prompt: "风格：毒舌。", DefaultEmotion: brain.EmotionTease},
	{ID: "gentle", Name: "温柔陪伴", Aliases: []string{"温柔"}, Prompt: "风格：温柔。", DefaultEmotion: brain.EmotionGentle},
}

type memoryState struct {
	mu sync.Mutex
	id string
}

func (m *memoryState) LoadPersona() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.id, nil
}
func (m *memoryState) SavePersona(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.id = id
	return nil
}

func newPersonaTalker(t *testing.T, model brain.ChatModel, state brain.PersonaState, seen *[]brain.Utterance) *talker {
	t.Helper()
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{seen: seen}},
		brain.Options{MaxHistoryTurn: 6, Personas: testPersonas, DefaultPersona: "buddy", PersonaState: state})
	k.send(realtimeproto.SessionUpdate{Instructions: "基础规则。"})
	return k
}

// 系统提示词 = 基础提示词 + 当前性格；切换后：这次回答剩下的部分、之后每一轮都用新性格；记住、重启后保持。
func TestSwitchPersona(t *testing.T) {
	model := braintest.NewModel(t,
		braintest.Say("好的。"),
		braintest.Call("switch_persona", map[string]any{"name": "温柔陪伴"}), braintest.Say("好的。"),
		braintest.Say("好的。"),
		braintest.Say("好的。"))
	state := &memoryState{}
	var seen []brain.Utterance
	k := newPersonaTalker(t, model, state, &seen)

	k.say("你好")
	if got := model.LastRequest().System(); got != "基础规则。\n\n风格：毒舌。" {
		t.Fatalf("默认性格的系统提示词 = %q", got)
	}
	if len(seen) != 1 || seen[0].Emotion != brain.EmotionTease {
		t.Errorf("没打标记的句子应该用毒舌搭子的默认语气（调侃）：%+v", seen)
	}

	seen = nil
	k.say("换成温柔模式")
	if got := model.LastRequest().System(); !strings.HasSuffix(got, "风格：温柔。") {
		t.Errorf("切换后同一次回答的下一轮就该用新性格：%q", got)
	}
	if id, _ := state.LoadPersona(); id != "gentle" {
		t.Errorf("应该记住新性格，记的是 %q", id)
	}
	if len(seen) == 0 || seen[0].Emotion != brain.EmotionGentle {
		t.Errorf("切换后的回答应该用温柔陪伴的默认语气：%+v", seen)
	}

	k.say("今天好累")
	if got := model.LastRequest().System(); !strings.HasSuffix(got, "风格：温柔。") {
		t.Errorf("之后每一轮都用新性格：%q", got)
	}

	// 「重启」：新会话从状态里恢复
	restarted := newPersonaTalker(t, model, state, nil)
	restarted.say("你好")
	if got := model.LastRequest().System(); !strings.HasSuffix(got, "风格：温柔。") {
		t.Errorf("重启后应保持上次的性格：%q", got)
	}
}

// 只给了一个性格、或者没有性格档案时，不提供 switch_persona。
func TestSwitchPersonaToolOnlyWithChoices(t *testing.T) {
	model := &specRecorder{}
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{}},
		brain.Options{MaxHistoryTurn: 6, Personas: testPersonas[:1]})
	k.say("你好")
	if slices.Contains(model.names, "switch_persona") {
		t.Error("只有一个性格时不应提供 switch_persona")
	}
}

type specRecorder struct{ names []string }

func (m *specRecorder) StreamReply(_ context.Context, _ []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	for _, tool := range tools {
		m.names = append(m.names, tool.Name)
	}
	return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{Text: "好的。"}, nil) }
}
func (m *specRecorder) Warmup(context.Context) error { return nil }

// 终端声明没有摄像头：不提供 look_at_camera，系统提示词末尾说明看不到。
func TestNoCameraHidesLookTool(t *testing.T) {
	model := &promptRecorder{}
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{}},
		brain.Options{MaxHistoryTurn: 6, Instructions: "基础规则。"})

	k.say("你好")
	if !slices.Contains(model.tools, "look_at_camera") {
		t.Fatalf("默认（有摄像头）应提供 look_at_camera：%v", model.tools)
	}
	k.send(realtimeproto.SessionUpdate{NoCamera: true})
	k.say("你能看到我吗")
	if slices.Contains(model.tools, "look_at_camera") {
		t.Errorf("没有摄像头时不应提供 look_at_camera：%v", model.tools)
	}
	if !strings.HasPrefix(model.system, "基础规则。") || !strings.Contains(model.system, "没有摄像头") {
		t.Errorf("系统提示词应保留基础规则并说明没有摄像头：%q", model.system)
	}
}

// 摄像头被拔掉（设备报告里没有摄像头）就不给模型看画面的工具，插回来再给；
// session.update 说「有摄像头」，也不能盖掉设备报告的「现在不可用」。
func TestCameraComesAndGoesWithDeviceReports(t *testing.T) {
	model := &promptRecorder{}
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{}},
		brain.Options{MaxHistoryTurn: 6, Instructions: "基础规则。"})

	k.send(realtimeproto.HaroTerminalDevices{Microphone: "USB", Speaker: "音箱"}) // 摄像头断着
	k.send(realtimeproto.SessionUpdate{NoCamera: false})                        // 终端本身有摄像头
	k.say("你看得见我吗")
	if slices.Contains(model.tools, "look_at_camera") || !strings.Contains(model.system, "没有摄像头") {
		t.Fatalf("摄像头不可用时不应提供 look_at_camera：%v", model.tools)
	}
	k.send(realtimeproto.HaroTerminalDevices{Microphone: "USB", Speaker: "音箱", Camera: "EMEET"})
	k.say("现在呢")
	if !slices.Contains(model.tools, "look_at_camera") || strings.Contains(model.system, "没有摄像头") {
		t.Errorf("摄像头插回来后应恢复 look_at_camera：%v", model.tools)
	}
}

// promptRecorder 记下最近一次收到的工具名和系统提示词。
type promptRecorder struct {
	tools  []string
	system string
}

func (m *promptRecorder) StreamReply(_ context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.tools = nil
	for _, tool := range tools {
		m.tools = append(m.tools, tool.Name)
	}
	m.system = braintest.Request{Messages: messages}.System()
	return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{Text: "好的。"}, nil) }
}
func (m *promptRecorder) Warmup(context.Context) error { return nil }
