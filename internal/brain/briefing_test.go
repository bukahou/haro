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

// reportBriefer 附上一条要「说完才算告诉了」的情况，记下确认了几次、被打断了几次。
type reportBriefer struct {
	mu          sync.Mutex
	delivered   int
	interrupted int
}

func (b *reportBriefer) Brief(string) brain.Briefing {
	return brain.Briefing{Lines: []string{"后台调查查完了：结论是 A"}, Delivered: func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.delivered++
	}, Interrupted: func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.interrupted++
	}}
}

func (b *reportBriefer) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.delivered
}

func (b *reportBriefer) cuts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.interrupted
}

// 附上的情况：这一轮说完、主人也听完了（下一轮开始时还没收到截断）才确认告诉了；
// 被打断、出错时不确认（下一轮照样附上）。声音在终端播完要比服务端发完晚得多，说完时还不知道主人听没听完。
func TestBriefingDeliveredOnlyWhenHeard(t *testing.T) {
	briefer := &reportBriefer{}
	model := braintest.NewModel(t, braintest.Reply{Chunks: []string{"结论是"}, Hang: true}, braintest.Reply{Err: errors.New("503")},
		braintest.Say("结论是 A。"), braintest.Say("好。"))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Briefers: []brain.Briefer{briefer}},
		brain.Options{MaxHistoryTurn: 6})

	interruptWhen(t, k, func() bool { return len(model.Requests()) >= 1 }, 0)
	braintest.Done(t, k.ask("有什么事吗"))
	braintest.Done(t, k.ask("有什么事吗"))
	if got := briefer.count(); got != 0 {
		t.Fatalf("被打断、出错的轮次不应确认：%d", got)
	}
	if got := briefer.cuts(); got != 1 {
		t.Fatalf("生成时被打断的那一轮应通知被打断一次（出错的不算）：%d", got)
	}
	k.say("有什么事吗")
	if got := briefer.count(); got != 0 {
		t.Fatalf("说完了，但还不知道主人听没听完：不应确认（%d）", got)
	}
	k.say("嗯")
	if got := briefer.count(); got != 1 {
		t.Errorf("下一轮开始时没收到截断（上一轮播完了）：应确认一次（%d）", got)
	}
}

// 说完了、声音还在终端播的时候被打断：附上的情况没听完，不算告诉了（之后照样附上）。
func TestBriefingNotDeliveredWhenPlaybackCut(t *testing.T) {
	briefer := &reportBriefer{}
	model := braintest.NewModel(t, braintest.Say("结论是 A。"), braintest.Say("我在。"))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Briefers: []brain.Briefer{briefer}},
		brain.Options{MaxHistoryTurn: 6})

	k.say("有什么事吗")
	k.send(realtimeproto.ConversationItemTruncate{AudioEndMs: 100})
	k.say("哈罗哈罗")
	if got := briefer.count(); got != 0 {
		t.Errorf("上一轮播放中被打断：不应确认（%d）", got)
	}
	if got := briefer.cuts(); got != 1 {
		t.Errorf("上一轮播放中被打断：应通知被打断一次（%d）", got)
	}
}

// 说完之后会话就结束了（终端断开）：不知道主人听没听完，不算告诉了（下次再说一遍，不漏）。
func TestBriefingNotDeliveredWhenSessionEnds(t *testing.T) {
	briefer := &reportBriefer{}
	model := braintest.NewModel(t, braintest.Say("结论是 A。"))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Briefers: []brain.Briefer{briefer}},
		brain.Options{MaxHistoryTurn: 6})

	k.say("有什么事吗")
	_ = k.session.Close()
	if got := briefer.count(); got != 0 {
		t.Errorf("会话结束了、没等到下一轮：不应确认（%d）", got)
	}
}

// deliveringToolbox 的 report 工具像 research_status：取出结论交给模型，并登记「主人听完才算告诉了」。
type deliveringToolbox struct {
	mu    sync.Mutex
	heard int
}

func (b *deliveringToolbox) Specs() []brain.ToolSpec {
	return []brain.ToolSpec{{Name: "report", Parameters: []byte(`{}`)}}
}

func (b *deliveringToolbox) Call(ctx context.Context, _ brain.ToolCall) (string, error) {
	brain.OnDelivered(ctx, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.heard++
	})
	return "结论是 A", nil
}

func (b *deliveringToolbox) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.heard
}

// 工具登记的事和附上的情况同一个时机确认：这一轮说完、主人听完；被打断（生成中或播放中）都不确认。
func TestToolDeliveredOnlyWhenHeard(t *testing.T) {
	for _, c := range []struct {
		name  string
		after func(k *talker, model *braintest.Model)
		want  int
	}{
		{"说完、播完", func(*talker, *braintest.Model) {}, 1},
		{"说完、播放中被打断", func(k *talker, _ *braintest.Model) {
			k.send(realtimeproto.ConversationItemTruncate{AudioEndMs: 100})
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			toolbox := &deliveringToolbox{}
			model := braintest.NewModel(t, braintest.Call("report", nil), braintest.Say("结论是 A。"), braintest.Say("好。"))
			k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Tools: toolbox},
				brain.Options{MaxHistoryTurn: 6})
			k.say("调查有结果了吗")
			if got := toolbox.count(); got != 0 {
				t.Fatalf("说完时还不知道主人听没听完：不应确认（%d）", got)
			}
			c.after(k, model)
			k.say("嗯")
			if got := toolbox.count(); got != c.want {
				t.Errorf("want %d，got %d", c.want, got)
			}
		})
	}

	// 生成中被打断：这一轮不算说完，登记的事作废
	toolbox := &deliveringToolbox{}
	model := braintest.NewModel(t, braintest.Call("report", nil), braintest.Reply{Chunks: []string{"结论是"}, Hang: true},
		braintest.Say("好。"))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Tools: toolbox}, brain.Options{MaxHistoryTurn: 6})
	interruptWhen(t, k, func() bool { return len(model.Requests()) >= 2 }, 0)
	braintest.Done(t, k.ask("调查有结果了吗"))
	k.say("嗯")
	if got := toolbox.count(); got != 0 {
		t.Errorf("生成中被打断：不应确认（%d）", got)
	}
}

// 每轮的即时情况附在主人这句话前面（不放系统提示词：系统提示词与历史每轮保持不变，提示词缓存才命中得了），历史里只存原话。
func TestBriefingGoesBeforeTheUserTurn(t *testing.T) {
	model := &fakeModel{tokens: []string{"好的。"}}
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model, Instructions: "你是测试助手"}, Synthesizer: fakeSynthesizer{},
		Briefers: []brain.Briefer{staticBriefer{lines: []string{"额度提醒：这个月用到 80% 了"}}}},
		brain.Options{MaxHistoryTurn: 6})
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
	if history := withoutSynthetic(model.history); len(history) < 2 || history[0].Text != "你好" {
		t.Errorf("历史里只存原话：%+v", model.history)
	}
}

// 模型返回 SpokenError（例如额度用完）时，服务端把原因说出来，而不是只报错。
type overBudgetModel struct{}

type spokenErr struct{}

func (spokenErr) Error() string { return "over" }
func (spokenErr) Phrase() (string, map[string]string) {
	return brain.PhraseOverMonthlyBudget, map[string]string{"spent": "3100", "limit": "3000", brain.PhraseCurrency: "JPY"}
}

func (overBudgetModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{}, spokenErr{}) }
}

// 念哪种语言跟着主人最近一句话识别出的语种：没有这种语言（或不知道）时用默认的。
func TestSpokenErrorIsSaidAloud(t *testing.T) {
	var said []brain.Utterance
	k := newTestTalker(t, overBudgetModel{}, fakeSynthesizer{seen: &said})
	for _, c := range []struct {
		language, text, want string
	}{
		{"", "你好", "这个月的费用（大模型和搜索）已经约 3100 日元，到上限 3000 日元了，这个月先不回答了。"},
		{"ja", "こんにちは", "今月の費用（AI と検索）がおよそ 3100 円で、上限の 3000 円に達したので、今月はもうお答えできません。"},
		{"ko", "안녕하세요", "这个月的费用（大模型和搜索）已经约 3100 日元，到上限 3000 日元了，这个月先不回答了。"},
	} {
		said = nil
		done := braintest.Done(t, braintest.Ask(t, k.session, k.stream, braintest.Utterance{Text: c.text, Language: c.language}))
		if done.Status != realtimeproto.StatusCompleted || len(said) != 1 || said[0].Text != c.want {
			t.Errorf("语种 %q：应念出原因：status=%v said=%+v", c.language, done.Status, said)
		}
	}
}

type staticProfiles map[string]string

func (p staticProfiles) Profile(person string) string { return p[person] }

// 主人资料放在系统提示词里：内置规矩之后、性格之前；按说话人选。
func TestProfileGoesIntoSystemPromptAfterInstructions(t *testing.T) {
	model := &fakeModel{tokens: []string{"好的。"}}
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model, Instructions: "你是 Haro。\n\n追加的一条规矩"},
		Synthesizer: fakeSynthesizer{}, Profiles: staticProfiles{brain.SharedPerson: "【主人资料】不吃香菜"}},
		brain.Options{MaxHistoryTurn: 6})
	k.ask("你好")
	system := model.history[0].Text
	builtin := strings.Index(system, "你是 Haro")
	extra := strings.Index(system, "追加的一条规矩")
	profile := strings.Index(system, "不吃香菜")
	if builtin != 0 || extra < builtin || profile < extra {
		t.Errorf("顺序应为 内置规矩 → 外部追加 → 主人资料：%d %d %d\n%s", builtin, extra, profile, system)
	}
}
