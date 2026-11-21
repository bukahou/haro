package brain_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// fakeToolbox 记录调用（和参数）；delete_note 会破坏、要过确认闸。
type fakeToolbox struct {
	mu        sync.Mutex
	calls     []string
	arguments []map[string]any
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
	f.arguments = append(f.arguments, call.Arguments)
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

// argumentsOf 是第 i 次调用的参数。
func (f *fakeToolbox) argumentsOf(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.arguments[i]
}

func newToolTalker(t *testing.T, model brain.ChatModel, toolbox brain.Toolbox) *talker {
	t.Helper()
	parts := brain.Components{Model: model, Synthesizer: fakeSynthesizer{}}
	if toolbox != nil {
		parts.Tools = toolbox
	}
	return newTalker(t, parts, brain.Options{MaxHistoryTurn: 6})
}

var searchCarSpot = braintest.Call("search_notes", map[string]any{"query": "车停在哪"})

func TestToolLoopReadOnlyTool(t *testing.T) {
	model := braintest.NewModel(t, searchCarSpot, braintest.Say("查到了：车停在C3的7号位。"))
	toolbox := &fakeToolbox{}
	k := newToolTalker(t, model, toolbox)

	if spoken := k.say("我的车停在哪了？"); spoken != "查到了：车停在C3的7号位。" {
		t.Errorf("播出 %q", spoken)
	}
	if got := toolbox.called(); !slices.Equal(got, []string{"search_notes"}) {
		t.Errorf("工具调用 %v", got)
	}
	// 第二次请求把工具往来交回模型：最后是 assistant(tool_calls) + tool(result)
	second := model.Requests()[1].Messages
	n := len(second)
	if n < 2 || second[n-2].Role != brain.RoleAssistant || len(second[n-2].ToolCalls) != 1 ||
		second[n-1].Role != brain.RoleTool || second[n-1].ToolName != "search_notes" || second[n-1].Text != "车停在C3的7号位" {
		t.Errorf("工具往来没有正确交回模型：%+v", second[max(n-2, 0):])
	}
}

// 调工具时一次回答请求了模型两次：用量要两次加起来，不能只算最后一次。
func TestToolLoopUsageAddsUpAllRequests(t *testing.T) {
	usage := &brain.ReplyUsage{PromptTokens: 100, OutputTokens: 5}
	model := braintest.NewModel(t,
		braintest.Reply{ToolCalls: searchCarSpot.ToolCalls, Usage: usage},
		braintest.Reply{Chunks: []string{"查到了。"}, Usage: usage})
	k := newToolTalker(t, model, &fakeToolbox{})

	done := braintest.Done(t, k.ask("我的车停在哪了？"))
	if done.Usage == nil || done.Usage.InputTokens != 200 || done.Usage.OutputTokens != 10 {
		t.Errorf("用量应为两次请求之和（输入 200、输出 10）：%+v", done.Usage)
	}
}

// 模型每一轮都要求调用工具：第 4 轮工具定义照给（提示词缓存不失效），但声明不许调用，逼它直接回答。
func TestToolLoopStopsAfterMaxRounds(t *testing.T) {
	search := braintest.Call("search_notes", nil)
	model := braintest.NewModel(t, search, search, search, braintest.Say("没查到。"))
	k := newToolTalker(t, model, &fakeToolbox{})
	k.say("查一下")
	requests := model.Requests()
	if len(requests) != 4 {
		t.Fatalf("应请求 4 次：%d", len(requests))
	}
	for i, request := range requests {
		forbid := i == 3
		if !slices.Equal(request.ToolNames(), requests[0].ToolNames()) || len(request.Tools) == 0 || request.ForbidToolCalls != forbid {
			t.Errorf("第 %d 次：工具 %v、不许调用=%v；应为每次同样的工具，只有最后一次不许调用", i+1, request.ToolNames(), request.ForbidToolCalls)
		}
	}
}

// 不许调用工具的一轮模型还是要求调用：不执行，说了的话就是这一轮的回答。
func TestToolCallsInForbiddenRoundAreNotExecuted(t *testing.T) {
	search := braintest.Call("search_notes", nil)
	stubborn := braintest.Reply{Chunks: []string{"没查到。"}, ToolCalls: search.ToolCalls}
	model := braintest.NewModel(t, search, search, search, stubborn)
	toolbox := &fakeToolbox{}
	k := newToolTalker(t, model, toolbox)
	if answer := k.say("查一下"); answer != "没查到。" {
		t.Errorf("说了的话就是回答：%q", answer)
	}
	if calls := toolbox.called(); len(calls) != 3 {
		t.Errorf("不许调用的那一轮不该执行工具：执行了 %v", calls)
	}
}

// 回答到了输出上限：说出去的照常念完，终端收到「没说完」（incomplete，原因 max_output_tokens）；
// 历史里记下说了的部分和「被截断」的说明，模型下一轮知道自己没说完。
func TestTruncatedAnswerIsReportedAsIncomplete(t *testing.T) {
	model := braintest.NewModel(t, braintest.Reply{Chunks: []string{"从前有座山，山里有座庙。"}, Truncated: true}, braintest.Say("好的。"))
	k := newToolTalker(t, model, &fakeToolbox{})
	done := braintest.Done(t, k.ask("讲个长故事"))
	if done.Status != realtimeproto.StatusIncomplete || done.Reason != realtimeproto.ReasonMaxOutputTokens {
		t.Errorf("应报没说完：%+v", done)
	}
	k.say("然后呢")
	history := withoutSynthetic(model.LastRequest().Messages)
	answer := history[len(history)-2]
	if answer.Role != brain.RoleAssistant || !strings.Contains(answer.Text, "从前有座山，山里有座庙。") || !strings.Contains(answer.Text, "输出上限") {
		t.Errorf("历史里应记下说了的部分和被截断的说明：%+v", answer)
	}
}

func TestToolExchangeIsKeptInHistory(t *testing.T) {
	model := braintest.NewModel(t, searchCarSpot, braintest.Say("在C3。"), braintest.Say("不客气。"))
	k := newToolTalker(t, model, &fakeToolbox{})
	k.say("我的车停在哪了？")
	k.say("谢谢")

	var roles []brain.ChatRole
	for _, msg := range withoutSynthetic(model.LastRequest().Messages) {
		roles = append(roles, msg.Role)
	}
	// user(车停在哪), assistant(tool_calls), tool(结果), assistant(回答), user(谢谢)（这个测试会话没有设系统提示词；时间标记不算）
	want := []brain.ChatRole{brain.RoleUser, brain.RoleAssistant, brain.RoleTool, brain.RoleAssistant, brain.RoleUser}
	if !slices.Equal(roles, want) {
		t.Fatalf("第二轮的消息角色 %v，应为 %v（上一轮的工具往来要留在历史里）", roles, want)
	}
}

var lookAtCamera = braintest.Call("look_at_camera", nil)

func newCameraTalker(t *testing.T, model brain.ChatModel, camera brain.TerminalCamera, expert brain.FrameExpert) *talker {
	t.Helper()
	return newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{}, Camera: camera, FrameExpert: expert},
		brain.Options{MaxHistoryTurn: 6, Instructions: "基础规则。"})
}

// 模型调用 look_at_camera 时才经 MCP 让终端拍：拍到的画面交给模型；闲聊时不拍，历史里不留图片。
func TestLookAtCameraCapturesOnDemand(t *testing.T) {
	model := braintest.NewModel(t, lookAtCamera, braintest.Say("你拿着手机。"), braintest.Say("哈哈。"))
	camera := braintest.NewCamera([]byte("手机"))
	k := newCameraTalker(t, model, camera, nil)

	if spoken := braintest.Spoken(k.ask("我手里拿着什么？")); spoken != "你拿着手机。" {
		t.Fatalf("播出 %q", spoken)
	}
	if last := model.Requests()[1].Last(); last.Role != brain.RoleUser || string(last.Image) != "手机" {
		t.Fatalf("调用 look_at_camera 后，下一次请求应带上画面：%+v", last)
	}

	k.ask("讲个笑话吧")
	if camera.Captures() != 1 {
		t.Errorf("只在模型要看时拍一次，实际拍了 %d 次", camera.Captures())
	}
	last := model.LastRequest().Messages
	for _, msg := range last {
		if len(msg.Image) > 0 {
			t.Fatalf("闲聊时不应带画面，历史里也不应留图片：%+v", msg)
		}
	}
	// 历史里应留下「当时看了画面」的替身，而不是图片
	if !slices.ContainsFunc(last, func(msg brain.ChatMessage) bool { return msg.Text == brain.FrameRecalledNote }) {
		t.Error("历史里应留下「当时看了摄像头画面」")
	}
}

// fakeExpert 是看画面的专家：记下拿到的问题，交出设好的结论。
type fakeExpert struct {
	mu        sync.Mutex
	questions []brain.FrameQuestion
	report    brain.FrameReport
	err       error
}

func (e *fakeExpert) ExamineFrame(_ context.Context, question brain.FrameQuestion) (brain.FrameReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.questions = append(e.questions, question)
	if e.err != nil {
		return brain.FrameReport{}, e.err
	}
	return e.report, nil
}

// 有专家、这一轮只调用了看画面：专家那句直接当回答念出来，不再请求前台模型；前台看不到画面。
// 历史里留下调用前那句、专家的结论（已经说了什么 + 细节）和念出来的那句，下一轮追问时都在。
func TestLookAtCameraSpeaksExpertSentence(t *testing.T) {
	conclusion := brain.FrameReport{Spoken: "看着像充电头。", Details: "白色方块，一面有插脚。"}
	lookFirst := braintest.Reply{Chunks: []string{"我看看。"}, ToolCalls: lookAtCamera.ToolCalls}
	model, expert := braintest.NewModel(t, lookFirst, braintest.Say("插脚是两根的。")), &fakeExpert{report: conclusion}
	k := newCameraTalker(t, model, braintest.NewCamera([]byte("充电头")), expert)

	if spoken := braintest.Spoken(k.ask("我手里拿着什么？")); spoken != "我看看。"+conclusion.Spoken {
		t.Fatalf("播出 %q", spoken)
	}
	if len(model.Requests()) != 1 {
		t.Fatalf("专家那句直接念：这一轮只请求前台一次，实际 %d 次", len(model.Requests()))
	}
	if len(expert.questions) != 1 || string(expert.questions[0].Frame) != "充电头" || expert.questions[0].Request != "我手里拿着什么？" {
		t.Fatalf("专家应拿到画面和主人的原话：%+v", expert.questions)
	}

	k.ask("插脚是几根？")
	history := model.LastRequest().Messages
	for _, msg := range history {
		if len(msg.Image) > 0 {
			t.Fatalf("有专家时前台不应看到画面：%+v", msg)
		}
	}
	if !slices.ContainsFunc(history, func(msg brain.ChatMessage) bool {
		return msg.Role == brain.RoleTool && msg.Text == brain.ExpertReport(conclusion, true)
	}) {
		t.Errorf("历史里应有专家的结论（已经说了什么 + 细节）：%+v", history)
	}
	if !slices.ContainsFunc(history, func(msg brain.ChatMessage) bool {
		return msg.Role == brain.RoleAssistant && strings.HasSuffix(msg.Text, conclusion.Spoken)
	}) {
		t.Errorf("历史里这一轮的回答应是念出来的那句：%+v", history)
	}
}

// 同时调用了别的工具：专家那句交给前台，和别的结果一起说。
func TestLookAtCameraWithOtherToolsLetsModelSpeak(t *testing.T) {
	conclusion := brain.FrameReport{Spoken: "看着像充电头。", Details: "白色方块。"}
	both := braintest.Reply{ToolCalls: []brain.ToolCall{{Name: "look_at_camera"}, {Name: "switch_persona", Arguments: map[string]any{"persona": "不存在"}}}}
	model := braintest.NewModel(t, both, braintest.Say("前台说的。"))
	k := newCameraTalker(t, model, braintest.NewCamera([]byte("充电头")), &fakeExpert{report: conclusion})

	if spoken := braintest.Spoken(k.ask("看看这是什么，再换个性格")); spoken != "前台说的。" {
		t.Fatalf("播出 %q", spoken)
	}
	if !slices.ContainsFunc(model.LastRequest().Messages, func(msg brain.ChatMessage) bool {
		return msg.Role == brain.RoleTool && msg.Text == brain.ExpertReport(conclusion, false)
	}) {
		t.Errorf("前台应拿到照着说的那句：%+v", model.LastRequest().Messages)
	}
	// 历史里不带「照这句说」这类只对这一轮的要求：记成专家的结论和细节
	model.Then(braintest.Say("好的。"))
	k.say("谢谢")
	if !slices.ContainsFunc(model.LastRequest().Messages, func(msg brain.ChatMessage) bool {
		return msg.Role == brain.RoleTool && msg.ToolName == "look_at_camera" && msg.Text == brain.ExpertHistoryRecord(conclusion, false)
	}) {
		t.Errorf("历史里的看画面结果应记成专家的结论和细节：%+v", model.LastRequest().Messages)
	}
}

// 专家没单独交那句：前台拿到细节自己压短。
func TestLookAtCameraWithoutExpertSentence(t *testing.T) {
	conclusion := brain.FrameReport{Details: "白色方块，看着像充电头。"}
	model := braintest.NewModel(t, lookAtCamera, braintest.Say("像充电头。"))
	k := newCameraTalker(t, model, braintest.NewCamera([]byte("充电头")), &fakeExpert{report: conclusion})

	if spoken := braintest.Spoken(k.ask("我手里拿着什么？")); spoken != "像充电头。" {
		t.Fatalf("播出 %q", spoken)
	}
	if last := model.LastRequest().Last(); last.Role != brain.RoleTool || last.Text != brain.ExpertReport(conclusion, false) {
		t.Errorf("工具结果应是让前台自己压短的说明：%+v", last)
	}
}

// 要说的那句和细节分开：那句在前，细节另起一段；直接念了和交给前台说，说明不同；只有细节时让前台自己压短。
func TestExpertReportLayout(t *testing.T) {
	conclusion := brain.FrameReport{Spoken: "看着像充电头。", Details: "白色方块。"}
	said, relayed := brain.ExpertReport(conclusion, true), brain.ExpertReport(conclusion, false)
	for _, text := range []string{said, relayed} {
		spokenAt, detailsAt := strings.Index(text, conclusion.Spoken), strings.Index(text, conclusion.Details)
		if spokenAt < 0 || detailsAt < spokenAt {
			t.Errorf("要说的那句应在细节前面：%q", text)
		}
	}
	if said == relayed {
		t.Error("直接念了和交给前台说，说明应不同")
	}
	detailsOnly := brain.ExpertReport(brain.FrameReport{Details: "白色方块，看着像充电头。"}, false)
	if !strings.HasSuffix(detailsOnly, "白色方块，看着像充电头。") || detailsOnly == relayed {
		t.Errorf("只有细节时应换成让前台自己压短的说明：%q", detailsOnly)
	}
}

// 专家出错时：退回把画面直接交给前台。
func TestLookAtCameraFallsBackWhenExpertFails(t *testing.T) {
	model := braintest.NewModel(t, lookAtCamera, braintest.Say("你拿着手机。"))
	k := newCameraTalker(t, model, braintest.NewCamera([]byte("手机")), &fakeExpert{err: errors.New("503")})

	k.ask("我手里拿着什么？")
	if last := model.LastRequest().Last(); last.Role != brain.RoleUser || string(last.Image) != "手机" {
		t.Fatalf("专家出错时应把画面直接交给前台：%+v", last)
	}
}

// 终端能拍时才给模型 look_at_camera：没有摄像头（普通客户端）、或终端的工具清单里没有「拍一帧」都不给；
// 系统提示词不随摄像头变化（看画面的说明在工具说明里），提示词缓存不受影响。
func TestLookAtCameraOnlyWhileTerminalCanCapture(t *testing.T) {
	offered := func(camera brain.TerminalCamera) (bool, string) {
		model := braintest.NewModel(t, braintest.Say("好的。"))
		k := newCameraTalker(t, model, camera, nil)
		k.ask("你好")
		return slices.Contains(model.LastRequest().ToolNames(), "look_at_camera"), model.LastRequest().System()
	}
	camera := braintest.NewCamera([]byte("画面"))
	withCamera, system := offered(camera)
	camera.SetAvailable(false)
	unplugged, unpluggedSystem := offered(camera)
	none, noneSystem := offered(nil)
	if !withCamera || unplugged || none {
		t.Errorf("能拍时才给：能拍 %v，拔掉 %v，没有摄像头 %v", withCamera, unplugged, none)
	}
	if system != unpluggedSystem || system != noneSystem {
		t.Errorf("系统提示词不该随摄像头变化：\n%q\n%q\n%q", system, unpluggedSystem, noneSystem)
	}
}

// 拍失败时：模型拿到明确的「没拍成」和原因（不是空图），也不带画面消息。
func TestCaptureFailureIsToldToModel(t *testing.T) {
	model := braintest.NewModel(t, lookAtCamera, braintest.Say("现在看不到。"))
	camera := braintest.NewCamera(nil)
	failure := errors.New("摄像头被别的程序占着")
	camera.FailWith(failure)
	k := newCameraTalker(t, model, camera, nil)
	k.ask("我手里拿着什么？")
	answered := model.LastRequest().Messages
	result := answered[len(answered)-1]
	if result.Role != brain.RoleTool || result.Text != brain.CaptureFailedNote(failure) {
		t.Errorf("工具结果应说明没拍成和原因：%+v", result)
	}
	for _, msg := range answered {
		if len(msg.Image) > 0 {
			t.Fatalf("拍失败时不该有画面消息：%+v", msg)
		}
	}
}

// 一次删多条：确认问句把每一条都列出来。
// 专家写那句时拿到当前性格的说明；直接念出来的那句用性格的语气。
func TestLookAtCameraUsesPersonaStyle(t *testing.T) {
	expert := &fakeExpert{report: brain.FrameReport{Spoken: "充电头，没跑了。"}}
	var seen []brain.Utterance
	model := braintest.NewModel(t, lookAtCamera)
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{seen: &seen}, Camera: braintest.NewCamera([]byte("充电头")),
		FrameExpert: expert}, brain.Options{MaxHistoryTurn: 6, Instructions: "基础规则。", Personas: testPersonas, DefaultPersona: "buddy"})

	k.ask("我手里拿着什么？")
	if len(expert.questions) != 1 || expert.questions[0].Style != testPersonas[0].Prompt {
		t.Fatalf("专家应拿到当前性格的说明：%+v", expert.questions)
	}
	if len(seen) != 1 || seen[0].Text != "充电头，没跑了。" || seen[0].Emotion != testPersonas[0].DefaultEmotion {
		t.Errorf("直接念的那句应用性格的语气：%+v", seen)
	}
}
