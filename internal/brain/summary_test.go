package brain

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// summaryModel 记下收到的请求，回一段固定摘要；truncated 时写到一半就到了输出上限。
type summaryModel struct {
	mu        sync.Mutex
	requests  []string
	systems   []string   // 每次请求的系统提示词
	chains    [][]string // 每次请求时的代理链
	reply     string
	truncated bool
}

func (m *summaryModel) StreamReply(ctx context.Context, request ChatRequest) iter.Seq2[ReplyDelta, error] {
	messages := request.Messages
	m.mu.Lock()
	m.requests = append(m.requests, messages[len(messages)-1].Text)
	m.systems = append(m.systems, messages[0].Text)
	m.chains = append(m.chains, AgentChain(ctx))
	m.mu.Unlock()
	return func(yield func(ReplyDelta, error) bool) {
		if !yield(ReplyDelta{Text: m.reply}, nil) {
			return
		}
		if m.truncated {
			yield(ReplyDelta{Stop: StopMaxTokens}, nil)
		}
	}
}

func (m *summaryModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func TestRollingSummary(t *testing.T) {
	book := NewConversationBook(nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	model := &summaryModel{reply: "主人的车停在 B2。"}
	book.SummarizeWith(Agent{Name: "摘要", Model: model, Instructions: "写摘要。"})
	conversation := book.Conversation("主人")
	turn := func(text string) ConversationTurn {
		return ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: text}, {Role: RoleAssistant, Text: "好的，" + text + "。"},
			{Role: RoleTool, Text: "工具结果不进摘要"}}}
	}
	for _, text := range []string{"车停在 B2", "第二句", "第三句", "第四句"} {
		conversation.appendTurn(turn(text), 2, 0) // 窗口 2 轮：第 3、4 轮时掉出 2 轮，还不够一批
	}
	if model.count() != 0 {
		t.Fatal("没攒够一批不该压缩")
	}
	if messages := conversation.messages(); len(messages) != 4*4 { // 每轮：时间标记 + 三条
		t.Errorf("还没压进摘要的照常带着：%d 条", len(messages))
	}
	conversation.appendTurn(turn("第五句"), 2, 0) // 掉出第 3 轮：够 3 轮了
	deadline := time.Now().Add(2 * time.Second)
	for conversation.currentSummary() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := conversation.currentSummary(); got != "主人的车停在 B2。" {
		t.Fatalf("摘要：%q", got)
	}
	request := model.requests[0]
	if !strings.Contains(request, "主人：车停在 B2") || strings.Contains(request, "工具结果") {
		t.Errorf("压缩的输入：%s", request)
	}
	if model.systems[0] != "写摘要。" || !slices.Equal(model.chains[0], []string{"摘要"}) {
		t.Errorf("应按代理「摘要」的系统提示词写、记进代理链：%q %v", model.systems[0], model.chains[0])
	}
	if messages := conversation.messages(); len(messages) != 2*4 {
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
		conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "x"}, {Role: RoleAssistant, Text: "y"}}}, 2, 0)
	}
	if len(conversation.messages()) != 2*3 || conversation.currentSummary() != "" {
		t.Error("没开摘要：掉出窗口的直接丢")
	}
}

// 摘要写到一半到了输出上限：半份摘要不用，旧摘要（这里是没有）和没压进去的对话照旧带着。
func TestTruncatedSummaryIsNotUsed(t *testing.T) {
	book := NewConversationBook(nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	model := &summaryModel{reply: "主人的车停在", truncated: true}
	book.SummarizeWith(Agent{Name: "摘要", Model: model, Instructions: "写摘要。"})
	conversation := book.Conversation("主人")
	for _, text := range []string{"车停在 B2", "第二句", "第三句", "第四句", "第五句"} {
		conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: text},
			{Role: RoleAssistant, Text: "好的。"}}}, 2, 0)
	}
	deadline := time.Now().Add(2 * time.Second)
	for model.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // 让压缩那一步收尾
	if model.count() == 0 {
		t.Fatal("够一批了，应该请求压缩")
	}
	if got := conversation.currentSummary(); got != "" {
		t.Errorf("被截断的半份摘要不该用：%q", got)
	}
	if messages := conversation.messages(); len(messages) != 5*3 { // 每轮：时间标记 + 两条，五轮都还在
		t.Errorf("没压进去的对话照旧带着：%d 条", len(messages))
	}
}

// 上下文太长、提前移出窗口的：不等攒够一批（summaryBatch），马上压缩。
func TestCompactionSummarizesRightAway(t *testing.T) {
	book := NewConversationBook(nil, Options{CompactContextTokens: 1000}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	model := &summaryModel{reply: "聊过第一句。"}
	book.SummarizeWith(Agent{Name: "摘要", Model: model, Instructions: "写摘要。"})
	conversation := book.Conversation("主人")
	turn := func(text string) ConversationTurn {
		return ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: text}, {Role: RoleAssistant, Text: "好。"}}}
	}
	conversation.appendTurn(turn("第一句"), 6, 100)
	conversation.appendTurn(turn("第二句"), 6, 100)
	conversation.appendTurn(turn("第三句"), 6, 1000) // 到了门槛：移出 1 轮（不到一批 3 轮）
	deadline := time.Now().Add(2 * time.Second)
	for conversation.currentSummary() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := conversation.currentSummary(); got != "聊过第一句。" || model.count() != 1 {
		t.Fatalf("提前移出窗口的应马上压缩：%q（压了 %d 次）", got, model.count())
	}
	if messages := conversation.messages(); len(messages) != 2*3 { // 每轮：时间标记 + 两条
		t.Errorf("压完只带最近两轮：%d 条", len(messages))
	}
}
