package brain_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
)

// strongAgent 是接手的代理（测试用）：带着同样的历史回答这一轮剩下的部分。
func strongAgent(model brain.ChatModel, maxSteps int) *brain.Agent {
	return &brain.Agent{Name: "强模型", Model: model, MaxSteps: maxSteps, RepeatLimit: 3, RepeatWindow: 4}
}

// newEscalationTalker：strong 不为 nil 时，主人纠正时可以交给它接手。
func newEscalationTalker(t *testing.T, front, strong brain.ChatModel) *talker {
	t.Helper()
	parts := brain.Components{Front: brain.Agent{Model: front, Instructions: "基础规则。"}, Synthesizer: fakeSynthesizer{}}
	if strong != nil {
		parts.Handoff = brain.Handoff{Agent: strongAgent(strong, 8), OnCorrection: true}
	}
	return newTalker(t, parts, brain.Options{MaxHistoryTurn: 6})
}

var askStronger = braintest.Call("ask_stronger_model", nil)

// 前台调用 ask_stronger_model 后，这一轮剩下的部分交给接手的代理：带着同样的历史和系统提示词，最后一条是交接说明；
// 下一轮回到前台。
func TestAskStrongerModelHandsOverRestOfTurn(t *testing.T) {
	front := braintest.NewModel(t, askStronger, braintest.Say("前台回答。"))
	strong := braintest.NewModel(t, braintest.Say("更强的模型重答。"))
	k := newEscalationTalker(t, front, strong)

	spoken := braintest.Spoken(k.ask("不对，那是耳机盒"))
	if !strings.Contains(spoken, "更强的模型重答") {
		t.Errorf("这一轮应由接手的代理回答：%q", spoken)
	}
	k.say("好了谢谢")
	if len(front.Requests()) != 2 || len(strong.Requests()) != 1 {
		t.Fatalf("前台应请求 2 次（交出去前、下一轮）、接手的 1 次：%d / %d", len(front.Requests()), len(strong.Requests()))
	}
	handed, first := strong.Requests()[0], front.Requests()[0]
	if handed.Last().Text != brain.EscalatedNote || handed.System() != first.System() {
		t.Errorf("接手的代理应沿用系统提示词、看到交接说明：%q", handed.Last().Text)
	}
	if handed.Input().Text != "不对，那是耳机盒" {
		t.Errorf("接手的代理应看到主人这一句：%q", handed.Input().Text)
	}
	if !slices.Contains(first.ToolNames(), "ask_stronger_model") {
		t.Errorf("配了接手的代理时应提供 ask_stronger_model：%v", first.ToolNames())
	}
}

// 已经交出去后接手的代理再调用：不再交接，提示它直接回答。
func TestAskStrongerModelTwice(t *testing.T) {
	front := braintest.NewModel(t, askStronger)
	strong := braintest.NewModel(t, askStronger, braintest.Say("更强的模型重答。"))
	k := newEscalationTalker(t, front, strong)

	k.say("不对，再想想")
	requests := strong.Requests()
	if len(requests) != 2 {
		t.Fatalf("接手的代理应请求 2 次，实际 %d", len(requests))
	}
	if first, second := requests[0].Last(), requests[1].Last(); first.Text != brain.EscalatedNote || second.Text != brain.AlreadyEscalatedNote {
		t.Errorf("第二次调用应提示直接回答：%q / %q", first.Text, second.Text)
	}
}

// 没配接手的代理时不提供这个工具；模型照着历史里见过的名字调用时也不执行（执行方自己守门）。
func TestAskStrongerModelOnlyWhenConfigured(t *testing.T) {
	front := braintest.NewModel(t, askStronger, braintest.Say("今天 19 号。"))
	k := newEscalationTalker(t, front, nil)
	k.say("今天几号")
	requests := front.Requests()
	if slices.Contains(requests[0].ToolNames(), "ask_stronger_model") {
		t.Errorf("没配接手的代理时不应提供：%v", requests[0].ToolNames())
	}
	if got := requests[1].Last().Text; !strings.Contains(got, "not_offered") {
		t.Errorf("没提供的工具不应执行：%q", got)
	}
}

// 交接说明只给这一轮接手的代理看；历史里记成中性的记录，否则之后的轮次会把「不要再调用这个工具」当成规矩，主人再纠正时就不交接了。
func TestEscalationNoteIsNotRememberedVerbatim(t *testing.T) {
	front := braintest.NewModel(t, askStronger, braintest.Say("前台回答。"))
	strong := braintest.NewModel(t, braintest.Say("更强的模型重答。"))
	k := newEscalationTalker(t, front, strong)

	k.say("不对，那是耳机盒")
	k.say("好了谢谢")
	for _, msg := range front.LastRequest().Messages {
		if msg.Role == brain.RoleTool && msg.ToolName == "ask_stronger_model" && msg.Text != brain.EscalatedRecord {
			t.Errorf("历史里的交接应记成中性记录：%q", msg.Text)
		}
	}
}

// 接手的代理的步数从交接时重新算，按它自己的上限：上限 2 步时，它的第 2 次请求不许调用工具。
func TestHandoffCountsStepsByItsOwnLimit(t *testing.T) {
	front := braintest.NewModel(t, searchFor("耳机盒"), askStronger)
	strong := braintest.NewModel(t, searchFor("耳机盒 型号"), braintest.Say("是 AirPods 的盒子。"))
	parts := brain.Components{Front: brain.Agent{Model: front}, Synthesizer: fakeSynthesizer{}, Tools: &fakeToolbox{},
		Handoff: brain.Handoff{Agent: strongAgent(strong, 2), OnCorrection: true}}
	k := newTalker(t, parts, brain.Options{MaxHistoryTurn: 6})
	k.say("不对，这是什么")
	requests := strong.Requests()
	if len(requests) != 2 || requests[0].ForbidToolCalls || !requests[1].ForbidToolCalls {
		t.Fatalf("接手的代理第 1 次可以调用、第 2 次（它的最后一步）不许调用：%+v", requests)
	}
}

// 接手的代理只拿到它的允许名单里的工具；名单外的调用不执行。
func TestHandoffOffersOnlyItsAllowedTools(t *testing.T) {
	front := braintest.NewModel(t, askStronger)
	strong := braintest.NewModel(t, searchFor("耳机盒"), braintest.Say("查不了。"))
	agent := strongAgent(strong, 8)
	agent.Tools = []string{"ask_stronger_model"} // 没有 search_notes
	parts := brain.Components{Front: brain.Agent{Model: front}, Synthesizer: fakeSynthesizer{}, Tools: &fakeToolbox{},
		Handoff: brain.Handoff{Agent: agent, OnCorrection: true}}
	k := newTalker(t, parts, brain.Options{MaxHistoryTurn: 6})
	k.say("不对，再查查")
	requests := strong.Requests()
	if len(requests) != 2 || !slices.Equal(requests[0].ToolNames(), []string{"ask_stronger_model"}) {
		t.Fatalf("接手的代理只应拿到名单里的工具：%v", requests[0].ToolNames())
	}
	if got := requests[1].Last().Text; !strings.Contains(got, "not_offered") {
		t.Errorf("名单外的调用不应执行：%q", got)
	}
	if !slices.Contains(front.Requests()[0].ToolNames(), "search_notes") {
		t.Errorf("前台没有名单，应拿到全部工具：%v", front.Requests()[0].ToolNames())
	}
}

// 画面要交给前台看时（没有看画面的专家），配了 OnFrame 就交给接手的代理看：带画面的请求由它回答。
func TestFrameHandsOverWhenConfigured(t *testing.T) {
	front := braintest.NewModel(t, lookAtCamera)
	strong := braintest.NewModel(t, braintest.Say("是耳机盒。"))
	parts := brain.Components{Front: brain.Agent{Model: front}, Synthesizer: fakeSynthesizer{}, Camera: braintest.NewCamera([]byte("耳机盒")),
		Handoff: brain.Handoff{Agent: strongAgent(strong, 8), OnFrame: true}}
	k := newTalker(t, parts, brain.Options{MaxHistoryTurn: 6})
	k.say("我手里拿着什么")
	requests := strong.Requests()
	if len(requests) != 1 || !slices.ContainsFunc(requests[0].Messages, func(msg brain.ChatMessage) bool { return len(msg.Image) > 0 }) {
		t.Fatalf("带画面的请求应交给接手的代理：%d 次", len(requests))
	}
	if slices.Contains(front.Requests()[0].ToolNames(), "ask_stronger_model") {
		t.Error("只配了 OnFrame 时不应提供 ask_stronger_model")
	}
}

// 代理定义里的工具名要是会话或工具箱提供的（服务端启动时据此检查，写错了就起不来）。
func TestUnknownToolsInAllowList(t *testing.T) {
	got := brain.UnknownTools([]string{"search_notes", "ask_stronger_model", "look_at_camera", "serch_notes"}, &fakeToolbox{})
	if !slices.Equal(got, []string{"serch_notes"}) {
		t.Errorf("只有写错的名字不认识：%v", got)
	}
	if got := brain.UnknownTools([]string{"search_notes"}, nil); !slices.Equal(got, []string{"search_notes"}) {
		t.Errorf("没有工具箱时工具箱的名字不认识：%v", got)
	}
}

// 代理链：接手换掉最后一个，子任务接在后面；超过上限时报错，不再往下交。
func TestAgentChainDepthIsLimited(t *testing.T) {
	ctx, err := brain.EnterAgent(t.Context(), quiet, "前台", brain.Subtask)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ = brain.EnterAgent(ctx, quiet, "强模型", brain.Handover)
	if got := brain.AgentChain(ctx); !slices.Equal(got, []string{"强模型"}) {
		t.Fatalf("接手应换掉最后一个：%v", got)
	}
	for i := 1; i < brain.MaxAgentDepth; i++ {
		if ctx, err = brain.EnterAgent(ctx, quiet, "子任务", brain.Subtask); err != nil {
			t.Fatalf("第 %d 层不应报错：%v", i+1, err)
		}
	}
	if _, err := brain.EnterAgent(ctx, quiet, "再深一层", brain.Subtask); !errors.Is(err, brain.ErrAgentTooDeep) {
		t.Errorf("超过 %d 层应报 ErrAgentTooDeep：%v", brain.MaxAgentDepth, err)
	}
}
