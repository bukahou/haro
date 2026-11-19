package brain_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
)

func newEscalationTalker(t *testing.T, model brain.ChatModel, escalation bool) *talker {
	t.Helper()
	return newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{}},
		brain.Options{MaxHistoryTurn: 6, Escalation: escalation})
}

// escalationFlags 是每次请求时本轮主人那句话上的 Escalate 标记。
func escalationFlags(model *braintest.Model) []bool {
	var flags []bool
	for _, request := range model.Requests() {
		flags = append(flags, request.Input().Escalate)
	}
	return flags
}

var askStronger = braintest.Call("ask_stronger_model", nil)

// 前台调用 ask_stronger_model 后，这一轮剩下的请求标上 Escalate（选模型的包装层据此换更强的模型）；下一轮不再带标记。
func TestAskStrongerModelEscalatesRestOfTurn(t *testing.T) {
	model := braintest.NewModel(t, askStronger, braintest.Say("更强的模型重答。"), braintest.Say("前台回答。"))
	k := newEscalationTalker(t, model, true)

	k.say("不对，那是耳机盒")
	k.say("好了谢谢")
	if got := escalationFlags(model); !slices.Equal(got, []bool{false, true, false}) {
		t.Errorf("各次请求的标记应为 [false true false]，实际 %v", got)
	}
	if !slices.Contains(model.Requests()[0].ToolNames(), "ask_stronger_model") {
		t.Errorf("配了更强的模型时应提供 ask_stronger_model：%v", model.Requests()[0].ToolNames())
	}
}

// 已经交给更强的模型后再调用：不重复标记，提示它直接回答。
func TestAskStrongerModelTwice(t *testing.T) {
	model := braintest.NewModel(t, askStronger, askStronger, braintest.Say("更强的模型重答。"))
	k := newEscalationTalker(t, model, true)

	k.say("不对，再想想")
	requests := model.Requests()
	if len(requests) != 3 {
		t.Fatalf("应请求 3 次，实际 %d", len(requests))
	}
	first, second := requests[1].Last(), requests[2].Last()
	if !strings.Contains(first.Text, "已改由更强的模型") || !strings.Contains(second.Text, "已经由更强的模型在回答") {
		t.Errorf("第二次调用应提示直接回答：%q / %q", first.Text, second.Text)
	}
}

// 没配更强的模型（Options.Escalation 为 false）时不提供这个工具。
func TestAskStrongerModelOnlyWhenConfigured(t *testing.T) {
	model := braintest.NewModel(t, braintest.Say("今天 19 号。"))
	k := newEscalationTalker(t, model, false)
	k.say("今天几号")
	if slices.Contains(model.LastRequest().ToolNames(), "ask_stronger_model") {
		t.Errorf("没配更强的模型时不应提供：%v", model.LastRequest().ToolNames())
	}
}
