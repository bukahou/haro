package brain_test

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/speech"
)

// escalatingModel 扮演前台模型 + 选模型的包装层：主人纠正时调用 ask_stronger_model（calls 次），
// 之后看本轮主人那句话有没有标 Escalate，标了就以「更强的模型」身份回答。记下每次请求时的标记与工具清单。
type escalatingModel struct {
	calls int

	mu        sync.Mutex
	escalated []bool
	offered   [][]string
	results   []string
}

func (m *escalatingModel) StreamReply(_ context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	escalated := false
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == brain.RoleUser && len(messages[i].Image) == 0 {
			escalated = messages[i].Escalate
			break
		}
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	last := messages[len(messages)-1]
	m.mu.Lock()
	m.escalated, m.offered = append(m.escalated, escalated), append(m.offered, names)
	if last.Role == brain.RoleTool {
		m.results = append(m.results, last.Text)
	}
	toolRounds := len(m.results)
	m.mu.Unlock()
	return func(yield func(brain.ReplyDelta, error) bool) {
		switch {
		case strings.HasPrefix(last.Text, "不对") && toolRounds < m.calls || last.Role == brain.RoleTool && toolRounds < m.calls:
			yield(brain.ReplyDelta{ToolCalls: []brain.ToolCall{{Name: "ask_stronger_model"}}}, nil)
		case escalated:
			yield(brain.ReplyDelta{Text: "更强的模型重答。"}, nil)
		default:
			yield(brain.ReplyDelta{Text: "前台回答。"}, nil)
		}
	}
}
func (m *escalatingModel) Warmup(context.Context) error { return nil }

func newEscalationSession(model brain.ChatModel, recognizer brain.SpeechRecognizer, escalation bool) *brain.Session {
	return brain.NewSession(brain.Components{
		Recognizer: recognizer, Model: model, Synthesizer: fakeSynthesizer{},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6, Escalation: escalation}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 前台调用 ask_stronger_model 后，这一轮剩下的请求标上 Escalate（选模型的包装层据此换更强的模型）；下一轮不再带标记。
func TestAskStrongerModelEscalatesRestOfTurn(t *testing.T) {
	model := &escalatingModel{calls: 1}
	recognizer := &switchableRecognizer{}
	session := newEscalationSession(model, recognizer, true)
	defer session.Close()

	recognizer.set("不对，那是耳机盒")
	if spoken := spokenText(askOneTurn(t, context.Background(), session, nil)); spoken != "更强的模型重答。" {
		t.Fatalf("调用后应由更强的模型回答，播出 %q", spoken)
	}
	recognizer.set("好了谢谢")
	if spoken := spokenText(askOneTurn(t, context.Background(), session, nil)); spoken != "前台回答。" {
		t.Fatalf("下一轮应回到前台，播出 %q", spoken)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if want := []bool{false, true, false}; len(model.escalated) != 3 || model.escalated[0] != want[0] ||
		model.escalated[1] != want[1] || model.escalated[2] != want[2] {
		t.Errorf("各次请求的标记应为 %v，实际 %v", want, model.escalated)
	}
	if !containsName(model.offered[0], "ask_stronger_model") {
		t.Errorf("配了更强的模型时应提供 ask_stronger_model：%v", model.offered[0])
	}
}

// 已经交给更强的模型后再调用：不重复标记，提示它直接回答。
func TestAskStrongerModelTwice(t *testing.T) {
	model := &escalatingModel{calls: 2}
	recognizer := &switchableRecognizer{}
	session := newEscalationSession(model, recognizer, true)
	defer session.Close()

	recognizer.set("不对，再想想")
	if spoken := spokenText(askOneTurn(t, context.Background(), session, nil)); spoken != "更强的模型重答。" {
		t.Fatalf("播出 %q", spoken)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.results) != 2 || !strings.Contains(model.results[0], "已改由更强的模型") || !strings.Contains(model.results[1], "已经由更强的模型在回答") {
		t.Errorf("第二次调用应提示直接回答：%v", model.results)
	}
}

// 没配更强的模型（Options.Escalation 为 false）时不提供这个工具。
func TestAskStrongerModelOnlyWhenConfigured(t *testing.T) {
	model := &escalatingModel{}
	recognizer := &switchableRecognizer{}
	session := newEscalationSession(model, recognizer, false)
	defer session.Close()

	recognizer.set("今天几号")
	askOneTurn(t, context.Background(), session, nil)
	model.mu.Lock()
	defer model.mu.Unlock()
	if containsName(model.offered[0], "ask_stronger_model") {
		t.Errorf("没配更强的模型时不应提供：%v", model.offered[0])
	}
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
