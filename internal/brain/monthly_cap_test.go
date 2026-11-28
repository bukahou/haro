package brain_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/money"
)

// capReached 是额度那一层交出的「这个月到上限了」（能念给主人，能认出是月度上限）。
type capReached struct{}

func (capReached) Error() string        { return "这个月到上限了" }
func (capReached) Is(target error) bool { return target == brain.ErrMonthlyCapReached }
func (capReached) Phrase() (string, map[string]string) {
	return brain.PhraseOverMonthlyBudgetRaisable, map[string]string{"spent": "3001", "limit": "3000", "raise": "500"}
}

// fakeCap 记下被提高了几次、每次带的主人的确认。
type fakeCap struct {
	mu            sync.Mutex
	raises        []string
	confirmations []brain.Confirmation
}

func (f *fakeCap) RaiseMonthlyCap(ctx context.Context, person string) (money.Amount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	confirmation, _ := brain.ConfirmationFrom(ctx)
	f.raises = append(f.raises, person)
	f.confirmations = append(f.confirmations, confirmation)
	return money.Of(3000+500*float64(len(f.raises)), "JPY"), nil
}

func (f *fakeCap) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.raises) }

// judgeInstructions 是测试里代理「额度判断」的正文。
const judgeInstructions = "只判断要不要提高上限、同不同意。"

func newCapTalker(t *testing.T, model, judge brain.ChatModel, cap brain.MonthlyCap) *talker {
	t.Helper()
	return newCapTalkerWith(t, model, &brain.Agent{Name: "额度判断", Model: judge, Instructions: judgeInstructions}, cap)
}

func newCapTalkerWith(t *testing.T, model brain.ChatModel, overBudget *brain.Agent, cap brain.MonthlyCap) *talker {
	t.Helper()
	return newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, MonthlyCap: cap, OverBudget: overBudget},
		brain.Options{MaxHistoryTurn: 6, MonthlyRaise: money.Of(500, "JPY")})
}

// 上限到了、主人说的不是要提高：不回答，念固定的话（到上限了、怎么提高）；判断用的模型说了话也不念。
func TestOverBudgetSpeaksFixedPhrase(t *testing.T) {
	model := braintest.NewModel(t, braintest.Reply{Err: capReached{}})
	judge := braintest.NewModel(t, braintest.Say("今天东京晴天，最高 18 度。"))
	cap := &fakeCap{}
	k := newCapTalker(t, model, judge, cap)
	spoken := braintest.Spoken(k.owner("今天天气怎么样"))
	if !strings.Contains(spoken, "到上限") || !strings.Contains(spoken, "500") || strings.Contains(spoken, "晴天") {
		t.Errorf("应只念固定的话（判断用的模型说的不念）：%q", spoken)
	}
	request := judge.LastRequest()
	if len(request.Messages) != 2 || request.Messages[0].Text != judgeInstructions || request.Last().Text != "今天天气怎么样" ||
		!slicesContains(request.ToolNames(), "raise_monthly_cap") {
		t.Errorf("判断用的调用用代理的正文、不带历史，只给提高上限的工具：%+v %v", request.Messages, request.ToolNames())
	}
	if cap.count() != 0 {
		t.Error("没要提高就不该提高")
	}
}

// 判断用的调用只给代理允许名单里的工具（覆盖目录里的定义去掉了提高上限：就不给它）。
func TestOverBudgetOffersOnlyAllowedTools(t *testing.T) {
	model := braintest.NewModel(t, braintest.Reply{Err: capReached{}})
	judge := braintest.NewModel(t, braintest.Say(""))
	k := newCapTalkerWith(t, model, &brain.Agent{Name: "额度判断", Model: judge, Instructions: judgeInstructions,
		Tools: []string{"confirm_action", "cancel_action"}}, &fakeCap{})
	k.owner("把上限提高一点")
	if names := judge.LastRequest().ToolNames(); slicesContains(names, "raise_monthly_cap") {
		t.Errorf("允许名单里没有提高上限：不该给它 %v", names)
	}
}

// 判断用的模型调用了没给它的工具（允许名单里没有提高上限）：不执行——不存待确认的操作，下一句确认那个编号也提高不了。
func TestOverBudgetIgnoresToolsNotOffered(t *testing.T) {
	model := braintest.NewModel(t, braintest.Reply{Err: capReached{}}, braintest.Reply{Err: capReached{}})
	judge := braintest.NewModel(t, braintest.Call("raise_monthly_cap", nil),
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}))
	cap := &fakeCap{}
	k := newCapTalkerWith(t, model, &brain.Agent{Name: "额度判断", Model: judge, Instructions: judgeInstructions,
		Tools: []string{"confirm_action", "cancel_action"}}, cap)
	k.owner("把上限提高一点")
	k.owner("可以")
	if cap.count() != 0 {
		t.Errorf("没交给它的工具不该执行：提高了 %d 次", cap.count())
	}
}

// 额度判断的正文里引用的工具名只填它允许名单里有的（nil = 全部）。
func TestOverBudgetPromptValuesFollowAllowList(t *testing.T) {
	all := brain.OverBudgetPromptValues(nil)
	if all["RaiseTool"] != "raise_monthly_cap" || all["ConfirmTool"] != "confirm_action" || all["CancelTool"] != "cancel_action" {
		t.Errorf("不限名单时三个都填：%v", all)
	}
	if only := brain.OverBudgetPromptValues([]string{"raise_monthly_cap"}); len(only) != 1 || only["RaiseTool"] != "raise_monthly_cap" {
		t.Errorf("只填名单里有的：%v", only)
	}
}

// 主人要提高：先问（固定的话），下一句同意了（由模型判断，测试直接指定它调用 confirm_action）才提高，念提高后的上限。
func TestOverBudgetRaiseNeedsConfirmation(t *testing.T) {
	model := braintest.NewModel(t, braintest.Reply{Err: capReached{}}, braintest.Reply{Err: capReached{}})
	judge := braintest.NewModel(t, braintest.Call("raise_monthly_cap", nil),
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}))
	cap := &fakeCap{}
	k := newCapTalker(t, model, judge, cap)

	if spoken := braintest.Spoken(k.owner("帮我把额度提高一点")); !strings.Contains(spoken, "500") || cap.count() != 0 {
		t.Fatalf("应先问要不要提高 500（还没提高）：%q 提高了 %d 次", spoken, cap.count())
	}
	if spoken := braintest.Spoken(k.owner("可以")); !strings.Contains(spoken, "3500") || cap.count() != 1 {
		t.Fatalf("同意后提高，并念提高后的上限：%q 提高了 %d 次", spoken, cap.count())
	}
	if got := cap.confirmations[0]; got != (brain.Confirmation{ActionID: "act_1", Tool: brain.RaiseMonthlyCapTool, Person: "主人"}) {
		t.Errorf("提高时要带上主人的确认（额度那一层据此守门）：%+v", got)
	}
	second := judge.Requests()[1]
	if !slicesContains(second.ToolNames(), "confirm_action") || !strings.Contains(second.Messages[1].Text, "act_1") {
		t.Errorf("第二句的判断要看得到待确认的操作：%+v %v", second.Messages, second.ToolNames())
	}
}

// 不是登记过声纹的人：不能提高（照样念到上限的话）。
func TestOverBudgetRaiseNeedsRegisteredVoice(t *testing.T) {
	model := braintest.NewModel(t, braintest.Reply{Err: capReached{}})
	judge := braintest.NewModel(t, braintest.Call("raise_monthly_cap", nil))
	cap := &fakeCap{}
	k := newCapTalker(t, model, judge, cap)
	spoken := braintest.Spoken(k.ask("把额度提高一点"))
	if !strings.Contains(spoken, "到上限") || cap.count() != 0 {
		t.Errorf("没认出是登记的人：不提高，念到上限的话：%q", spoken)
	}
}

// 平时（没到上限）也能口头提高：工具在清单里，照样过确认闸。
func TestRaiseToolOfferedNormally(t *testing.T) {
	model := braintest.NewModel(t, braintest.Say("好的。"))
	k := newCapTalker(t, model, braintest.NewModel(t), &fakeCap{})
	k.say("你好")
	if names := model.LastRequest().ToolNames(); !slicesContains(names, "raise_monthly_cap") {
		t.Errorf("接了上限时平时也该有提高上限的工具：%v", names)
	}
}

func slicesContains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
