package brain_test

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"strings"
	"testing"

	"haro/internal/brain"
	"haro/internal/realtimeproto"
	"haro/internal/speech"
)

type staticBriefer struct{ lines []string }

func (b staticBriefer) Brief(string) []string { return b.lines }

// 每轮的即时情况附在主人这句话前面（不放系统提示词：系统提示词与历史每轮保持不变，提示词缓存才命中得了），历史里只存原话。
func TestBriefingGoesBeforeTheUserTurn(t *testing.T) {
	model := &fakeModel{tokens: []string{"好的。"}}
	session := brain.NewSession(brain.Components{
		Recognizer: fakeRecognizer{text: "你好"}, Model: model, Synthesizer: fakeSynthesizer{},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
		Briefers:     []brain.Briefer{staticBriefer{lines: []string{"额度提醒：这个月用到 80% 了"}}},
	}, brain.Options{MaxHistoryTurn: 6}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer session.Close()
	ctx := context.Background()
	_ = session.Send(ctx, realtimeproto.SessionUpdate{Instructions: "你是测试助手"})
	askOneTurn(t, ctx, session, nil)
	firstSystem := model.history[0].Text

	if strings.Contains(firstSystem, "80%") || model.history[0].Role != brain.RoleSystem {
		t.Errorf("系统提示词里不该有即时情况：%q", firstSystem)
	}
	if model.lastInput.Role != brain.RoleUser || !strings.Contains(model.lastInput.Text, "用到 80% 了") ||
		!strings.HasSuffix(model.lastInput.Text, "【主人说】\n你好") {
		t.Errorf("主人这句话前面应附上即时情况：%q", model.lastInput.Text)
	}

	askOneTurn(t, ctx, session, nil) // 第二轮：系统提示词不变，历史里的上一句是原话
	if model.history[0].Text != firstSystem {
		t.Errorf("系统提示词每轮应保持不变")
	}
	if len(model.history) < 3 || model.history[1].Text != "你好" {
		t.Errorf("历史里只存原话：%+v", model.history)
	}
}

// 模型返回 SpokenError（例如额度用完）时，大脑把原因说出来，而不是只报错。
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
	session := newTestSession(fakeRecognizer{text: "你好"}, overBudgetModel{}, fakeSynthesizer{seen: &said})
	defer session.Close()
	events := askOneTurn(t, context.Background(), session, nil)
	done, _ := events[len(events)-1].(realtimeproto.ResponseDone)
	if done.Status != realtimeproto.StatusCompleted || len(said) != 1 || said[0].Text != "这个月的额度用完了。" {
		t.Errorf("应念出原因：status=%v said=%+v", done.Status, said)
	}
}
