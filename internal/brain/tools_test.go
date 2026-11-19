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
)

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

func TestToolLoopAsksConfirmationBeforeDestructiveTool(t *testing.T) {
	deleteSpot := braintest.Call("delete_note", map[string]any{"query": "车停车位"})
	model := braintest.NewModel(t,
		deleteSpot, braintest.Say("删好了。"), // 要删 → 主人确认 → 根据执行结果回答
		deleteSpot,                       // 要删 → 主人说算了：固定回答，不请求模型
		deleteSpot, braintest.Say("好的。")) // 要删 → 主人说了别的：放弃删除，照常回答
	toolbox := &fakeToolbox{}
	k := newToolTalker(t, model, toolbox)

	if spoken := k.say("把那条删了"); !strings.Contains(spoken, "确定要删掉和「车停车位」有关的那条吗") {
		t.Fatalf("应先问确认，播出 %q", spoken)
	}
	if len(toolbox.called()) != 0 {
		t.Fatal("确认之前不应执行删除")
	}
	if spoken := k.say("确认"); spoken != "删好了。" {
		t.Errorf("确认后应由模型根据删除结果回答，播出 %q", spoken)
	}
	if got := toolbox.called(); !slices.Equal(got, []string{"delete_note"}) {
		t.Errorf("确认后应执行删除：%v", got)
	}
	if last := model.LastRequest().Last(); last.Role != brain.RoleTool || last.Text != `{"deleted":true}` {
		t.Errorf("确认后交给模型的应是删除结果：%+v", last)
	}

	k.say("把那条删了")
	if spoken := k.say("算了"); spoken != "好的，不做了。" {
		t.Errorf("取消后播出 %q", spoken)
	}
	k.say("把那条删了")
	if spoken := k.say("今天天气怎么样？"); spoken != "好的。" {
		t.Errorf("说别的应照常回答，播出 %q", spoken)
	}
	if got := toolbox.called(); len(got) != 1 {
		t.Errorf("取消和说别的都不应执行删除：%v", got)
	}
	if model.Remaining() != 0 {
		t.Errorf("模型的请求次数不对：还剩 %d 条回答没用到", model.Remaining())
	}
}

// 模型每一轮都要求调用工具：第 4 轮不再提供工具，逼它直接回答。
func TestToolLoopStopsAfterMaxRounds(t *testing.T) {
	search := braintest.Call("search_notes", nil)
	model := braintest.NewModel(t, search, search, search, braintest.Say("没查到。"))
	k := newToolTalker(t, model, &fakeToolbox{})
	k.say("查一下")
	var offered []int
	for _, request := range model.Requests() {
		offered = append(offered, len(request.Tools))
	}
	if len(offered) != 4 || offered[0] == 0 || offered[3] != 0 {
		t.Errorf("每轮提供的工具数 %v，应为 3 轮有工具 + 最后 1 轮没有", offered)
	}
}

func TestToolExchangeIsKeptInHistory(t *testing.T) {
	model := braintest.NewModel(t, searchCarSpot, braintest.Say("在C3。"), braintest.Say("不客气。"))
	k := newToolTalker(t, model, &fakeToolbox{})
	k.say("我的车停在哪了？")
	k.say("谢谢")

	var roles []brain.ChatRole
	for _, msg := range model.LastRequest().Messages {
		roles = append(roles, msg.Role)
	}
	// user(车停在哪), assistant(tool_calls), tool(结果), assistant(回答), user(谢谢)（这个测试会话没有设系统提示词）
	want := []brain.ChatRole{brain.RoleUser, brain.RoleAssistant, brain.RoleTool, brain.RoleAssistant, brain.RoleUser}
	if !slices.Equal(roles, want) {
		t.Fatalf("第二轮的消息角色 %v，应为 %v（上一轮的工具往来要留在历史里）", roles, want)
	}
}

var lookAtCamera = braintest.Call("look_at_camera", nil)

func TestLookAtCameraOnDemand(t *testing.T) {
	model := braintest.NewModel(t, lookAtCamera, braintest.Say("你拿着手机。"), braintest.Say("哈哈。"))
	k := newToolTalker(t, model, nil) // 没有工具箱也能看画面

	if spoken := braintest.Spoken(k.askWithImage("我手里拿着什么？", []byte("手机"))); spoken != "你拿着手机。" {
		t.Fatalf("播出 %q", spoken)
	}
	if last := model.Requests()[1].Last(); last.Role != brain.RoleUser || string(last.Image) != "手机" {
		t.Fatalf("调用 look_at_camera 后，下一次请求应带上画面：%+v", last)
	}

	k.askWithImage("讲个笑话吧", []byte("手机"))
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

// fakeExpert 是看画面的专家：记下拿到的画面和主人的原话。
type fakeExpert struct {
	mu       sync.Mutex
	frames   []string
	requests []string
	err      error
}

func (e *fakeExpert) ExamineFrame(_ context.Context, frame []byte, request string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.frames, e.requests = append(e.frames, string(frame)), append(e.requests, request)
	if e.err != nil {
		return "", e.err
	}
	return "白色方块，看着像充电头。", nil
}

func newExpertTalker(t *testing.T, model brain.ChatModel, expert brain.FrameExpert) *talker {
	t.Helper()
	return newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{}, FrameExpert: expert},
		brain.Options{MaxHistoryTurn: 6})
}

// 有专家时：画面交给专家，前台只拿到专家的判断（文字），历史里留下这段判断。
func TestLookAtCameraAsksExpert(t *testing.T) {
	model, expert := braintest.NewModel(t, lookAtCamera, braintest.Say("转述专家的话。")), &fakeExpert{}
	k := newExpertTalker(t, model, expert)

	if spoken := braintest.Spoken(k.askWithImage("我手里拿着什么？", []byte("充电头"))); spoken != "转述专家的话。" {
		t.Fatalf("播出 %q", spoken)
	}
	if len(expert.frames) != 1 || expert.frames[0] != "充电头" || expert.requests[0] != "我手里拿着什么？" {
		t.Fatalf("专家应拿到画面和主人的原话：%v %v", expert.frames, expert.requests)
	}
	answered := model.LastRequest().Messages
	for _, msg := range answered {
		if len(msg.Image) > 0 {
			t.Fatalf("有专家时前台不应看到画面：%+v", msg)
		}
	}
	report := answered[len(answered)-1]
	if report.Role != brain.RoleTool || report.Text != brain.ExpertReportNote+"白色方块，看着像充电头。" {
		t.Errorf("工具结果应是专家的判断加转述要求：%+v", report)
	}
}

// 专家出错时：退回把画面直接交给前台。
func TestLookAtCameraFallsBackWhenExpertFails(t *testing.T) {
	model := braintest.NewModel(t, lookAtCamera, braintest.Say("你拿着手机。"))
	k := newExpertTalker(t, model, &fakeExpert{err: errors.New("503")})

	k.askWithImage("我手里拿着什么？", []byte("手机"))
	if last := model.LastRequest().Last(); last.Role != brain.RoleUser || string(last.Image) != "手机" {
		t.Fatalf("专家出错时应把画面直接交给前台：%+v", last)
	}
}

// 一次删多条：确认问句把每一条都列出来。
func TestConfirmationListsEveryTarget(t *testing.T) {
	model := braintest.NewModel(t,
		braintest.Call("delete_note", map[string]any{"queries": []any{"明天下午去取快递", "周三取干洗"}}))
	k := newToolTalker(t, model, &fakeToolbox{})
	if spoken := k.say("把取快递和取干洗删了"); !strings.Contains(spoken, "「明天下午去取快递」「周三取干洗」有关的这几条吗") {
		t.Errorf("应列出每一条，播出 %q", spoken)
	}
}
