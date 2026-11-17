package brain

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// summaryModel 记下收到的请求，回一段固定摘要。
type summaryModel struct {
	mu       sync.Mutex
	requests []string
	reply    string
}

func (m *summaryModel) StreamReply(_ context.Context, messages []ChatMessage, _ []ToolSpec) iter.Seq2[ReplyDelta, error] {
	m.mu.Lock()
	m.requests = append(m.requests, messages[len(messages)-1].Text)
	m.mu.Unlock()
	return func(yield func(ReplyDelta, error) bool) {
		yield(ReplyDelta{Text: "<think>想想</think>" + m.reply}, nil)
	}
}

func (m *summaryModel) Warmup(context.Context) error { return nil }

func (m *summaryModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func TestRollingSummary(t *testing.T) {
	book := NewConversationBook(nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	model := &summaryModel{reply: "主人的车停在 B2。"}
	book.SummarizeWith(model)
	conversation := book.Conversation("主人")
	turn := func(text string) []ChatMessage {
		return []ChatMessage{{Role: RoleUser, Text: text}, {Role: RoleAssistant, Text: "好的，" + text + "。"},
			{Role: RoleTool, Text: "工具结果不进摘要"}}
	}
	for _, text := range []string{"车停在 B2", "第二句", "第三句", "第四句"} {
		conversation.appendTurn(turn(text), 2) // 窗口 2 轮：第 3、4 轮时掉出 2 轮，还不够一批
	}
	if model.count() != 0 {
		t.Fatal("没攒够一批不该压缩")
	}
	if messages := conversation.messages(); len(messages) != 4*3 {
		t.Errorf("还没压进摘要的照常带着：%d 条", len(messages))
	}
	conversation.appendTurn(turn("第五句"), 2) // 掉出第 3 轮：够 3 轮了
	deadline := time.Now().Add(2 * time.Second)
	for conversation.currentSummary() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := conversation.currentSummary(); got != "主人的车停在 B2。" {
		t.Fatalf("摘要（去掉思考内容）：%q", got)
	}
	request := model.requests[0]
	if !strings.Contains(request, "主人：车停在 B2") || strings.Contains(request, "工具结果") {
		t.Errorf("压缩的输入：%s", request)
	}
	if messages := conversation.messages(); len(messages) != 2*3 {
		t.Errorf("压进摘要后只带窗口内的 2 轮：%d 条", len(messages))
	}
	if record := conversation.record(); record.Summary == "" || len(record.Unsummarized) != 0 {
		t.Errorf("摘要要存盘：%+v", record)
	}
	if note := summaryNote(conversation.currentSummary()); !strings.Contains(note, "B2") {
		t.Errorf("系统提示词里的摘要段：%q", note)
	}
}

func TestNoSummaryKeepsOldBehaviour(t *testing.T) {
	book := NewConversationBook(nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conversation := book.Conversation("主人")
	for i := 0; i < 5; i++ {
		conversation.appendTurn([]ChatMessage{{Role: RoleUser, Text: "x"}, {Role: RoleAssistant, Text: "y"}}, 2)
	}
	if len(conversation.messages()) != 4 || conversation.currentSummary() != "" {
		t.Error("没开摘要：掉出窗口的直接丢")
	}
}
