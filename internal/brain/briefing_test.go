package brain_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
)

type staticBriefer struct{ lines []string }

func (b staticBriefer) Brief(string) brain.Briefing { return brain.Briefing{Lines: b.lines} }

// reportBriefer 附上一条要「说完才算告诉了」的情况，记下确认了几次。
type reportBriefer struct {
	mu        sync.Mutex
	delivered int
}

func (b *reportBriefer) Brief(string) brain.Briefing {
	return brain.Briefing{Lines: []string{"后台调查查完了：结论是 A"}, Delivered: func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.delivered++
	}}
}

func (b *reportBriefer) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.delivered
}

// 附上的结论这一轮说完才确认告诉了；被打断、出错时不确认（下一轮照样附上）。
func TestBriefingDeliveredOnlyWhenTurnCompletes(t *testing.T) {
	briefer := &reportBriefer{}
	model := braintest.NewModel(t, braintest.Reply{Chunks: []string{"结论是"}, Hang: true}, braintest.Reply{Err: errors.New("503")},
		braintest.Say("结论是 A。"))
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{}, Briefers: []brain.Briefer{briefer}},
		brain.Options{MaxHistoryTurn: 6})

	interruptWhen(t, k, func() bool { return len(model.Requests()) >= 1 }, 0)
	braintest.Done(t, k.ask("有什么事吗"))
	braintest.Done(t, k.ask("有什么事吗"))
	if got := briefer.count(); got != 0 {
		t.Fatalf("被打断、出错的轮次不应确认：%d", got)
	}
	k.say("有什么事吗")
	if got := briefer.count(); got != 1 {
		t.Errorf("说完的那一轮应确认一次：%d", got)
	}
}

// 每轮的即时情况附在主人这句话前面（不放系统提示词：系统提示词与历史每轮保持不变，提示词缓存才命中得了），历史里只存原话。
func TestBriefingGoesBeforeTheUserTurn(t *testing.T) {
	model := &fakeModel{tokens: []string{"好的。"}}
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{},
		Briefers: []brain.Briefer{staticBriefer{lines: []string{"额度提醒：这个月用到 80% 了"}}}},
		brain.Options{MaxHistoryTurn: 6, Instructions: "你是测试助手"})
	k.ask("你好")
	firstSystem := model.history[0].Text

	if strings.Contains(firstSystem, "80%") || model.history[0].Role != brain.RoleSystem {
		t.Errorf("系统提示词里不该有即时情况：%q", firstSystem)
	}
	// 主人这句话是最后一条、原话；即时情况单独在它前面一条（标成系统合成，不是主人说的）
	situation := model.history[len(model.history)-1]
	if model.lastInput.Role != brain.RoleUser || model.lastInput.Text != "你好" || model.lastInput.Synthetic ||
		situation.Role != brain.RoleUser || !situation.Synthetic || !strings.Contains(situation.Text, "用到 80% 了") {
		t.Errorf("即时情况应单独一条、标成系统合成，后面是主人的原话：%+v / %+v", situation, model.lastInput)
	}

	k.ask("你好") // 第二轮：系统提示词不变，历史里的上一句是原话
	if model.history[0].Text != firstSystem {
		t.Errorf("系统提示词每轮应保持不变")
	}
	if len(model.history) < 3 || model.history[1].Text != "你好" {
		t.Errorf("历史里只存原话：%+v", model.history)
	}
}

// 模型返回 SpokenError（例如额度用完）时，服务端把原因说出来，而不是只报错。
type overBudgetModel struct{}

type spokenErr struct{}

func (spokenErr) Error() string         { return "over" }
func (spokenErr) SpokenMessage() string { return "这个月的额度用完了。" }

func (overBudgetModel) StreamReply(context.Context, []brain.ChatMessage, []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{}, spokenErr{}) }
}
func (overBudgetModel) Warmup(context.Context) error { return nil }

func TestSpokenErrorIsSaidAloud(t *testing.T) {
	var said []brain.Utterance
	k := newTestTalker(t, overBudgetModel{}, fakeSynthesizer{seen: &said})
	done := braintest.Done(t, k.ask("你好"))
	if done.Status != realtimeproto.StatusCompleted || len(said) != 1 || said[0].Text != "这个月的额度用完了。" {
		t.Errorf("应念出原因：status=%v said=%+v", done.Status, said)
	}
}

type staticProfiles map[string]string

func (p staticProfiles) Profile(person string) string { return p[person] }

// 主人资料放在系统提示词里：内置规矩之后、性格之前；按说话人选。
func TestProfileGoesIntoSystemPromptAfterInstructions(t *testing.T) {
	model := &fakeModel{tokens: []string{"好的。"}}
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{},
		Profiles: staticProfiles{brain.SharedPerson: "【主人资料】不吃香菜"}},
		brain.Options{MaxHistoryTurn: 6, Instructions: brain.ComposeInstructions("追加的一条规矩")})
	k.ask("你好")
	system := model.history[0].Text
	builtin := strings.Index(system, "你是 Haro")
	extra := strings.Index(system, "追加的一条规矩")
	profile := strings.Index(system, "不吃香菜")
	if builtin != 0 || extra < builtin || profile < extra {
		t.Errorf("顺序应为 内置规矩 → 外部追加 → 主人资料：%d %d %d\n%s", builtin, extra, profile, system)
	}
}
