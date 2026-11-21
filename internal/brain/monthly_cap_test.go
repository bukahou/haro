package brain_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
)

// capReached 是额度那一层交出的「这个月到上限了」（能念给主人，能认出是月度上限）。
type capReached struct{}

func (capReached) Error() string        { return "这个月到上限了" }
func (capReached) Is(target error) bool { return target == brain.ErrMonthlyCapReached }
func (capReached) Phrase() (string, map[string]string) {
	return brain.PhraseOverMonthlyBudgetRaisable, map[string]string{"spent": "3001", "limit": "3000", "raise": "500"}
}

// fakeCap 记下被提高了几次。
type fakeCap struct {
	mu     sync.Mutex
	raises []string
}

func (f *fakeCap) RaiseMonthlyCap(_ context.Context, person string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raises = append(f.raises, person)
	return 3000 + 500*float64(len(f.raises)), nil
}

func (f *fakeCap) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.raises) }

func newCapTalker(t *testing.T, model, judge brain.ChatModel, cap brain.MonthlyCap) *talker {
	t.Helper()
	return newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, MonthlyCap: cap, OverBudgetModel: judge},
		brain.Options{MaxHistoryTurn: 6, MonthlyRaiseYen: 500})
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
	if len(request.Messages) != 2 || request.Last().Text != "今天天气怎么样" || !slicesContains(request.ToolNames(), "raise_monthly_cap") {
		t.Errorf("判断用的调用不带历史，只给提高上限的工具：%+v %v", request.Messages, request.ToolNames())
	}
	if cap.count() != 0 {
		t.Error("没要提高就不该提高")
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
