package brain_test

import (
	"slices"
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
	if first.Text != brain.EscalatedNote || second.Text != brain.AlreadyEscalatedNote {
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

// 交接说明只给这一轮接手的模型看；历史里记成中性的记录，否则之后的轮次会把「不要再调用这个工具」当成规矩，主人再纠正时就不交接了。
func TestEscalationNoteIsNotRememberedVerbatim(t *testing.T) {
	model := braintest.NewModel(t, askStronger, braintest.Say("更强的模型重答。"), braintest.Say("前台回答。"))
	k := newEscalationTalker(t, model, true)

	k.say("不对，那是耳机盒")
	if got := model.Requests()[1].Last().Text; got != brain.EscalatedNote {
		t.Fatalf("这一轮接手的模型应看到交接说明：%q", got)
	}
	k.say("好了谢谢")
	for _, msg := range model.LastRequest().Messages {
		if msg.Role == brain.RoleTool && msg.ToolName == "ask_stronger_model" && msg.Text != brain.EscalatedRecord {
			t.Errorf("历史里的交接应记成中性记录：%q", msg.Text)
		}
	}
}
