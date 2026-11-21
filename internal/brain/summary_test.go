package brain

import (
	"context"
	"errors"
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
		stop := StopCompleted
		if m.truncated {
			stop = StopMaxTokens
		}
		yield(ReplyDelta{Stop: stop}, nil)
	}
}

func (m *summaryModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func TestRollingSummary(t *testing.T) {
	book := NewConversationBook(nil, Options{MaxHistoryTurn: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	model := &summaryModel{reply: "主人的车停在 B2。"}
	book.SummarizeWith(Agent{Name: "摘要", Model: model, Instructions: "写摘要。"})
	conversation := book.Conversation("主人")
	turn := func(text string) ConversationTurn {
		return ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: text}, {Role: RoleAssistant, Text: "好的，" + text + "。"},
			{Role: RoleTool, Text: "工具结果不进摘要"}}}
	}
	for _, text := range []string{"车停在 B2", "第二句", "第三句", "第四句"} {
		conversation.appendTurn(turn(text), 0) // 窗口 2 轮：第 3、4 轮时掉出 2 轮，还不够一批
	}
	if model.count() != 0 {
		t.Fatal("没攒够一批不该压缩")
	}
	if messages := conversation.messages(); len(messages) != 4*4 { // 每轮：时间标记 + 三条
		t.Errorf("还没压进摘要的照常带着：%d 条", len(messages))
	}
	conversation.appendTurn(turn("第五句"), 0) // 掉出第 3 轮：够 3 轮了
	deadline := time.Now().Add(2 * time.Second)
	for conversation.currentSummary() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := conversation.currentSummary(); got != "主人的车停在 B2。" {
		t.Fatalf("摘要：%q", got)
	}
	request := model.requests[0]
	if !strings.Contains(request, summaryOwnerLabel+"车停在 B2") || strings.Contains(request, "工具结果") {
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
	book := NewConversationBook(nil, Options{MaxHistoryTurn: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conversation := book.Conversation("主人")
	for i := 0; i < 5; i++ {
		conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "x"}, {Role: RoleAssistant, Text: "y"}}}, 0)
	}
	if len(conversation.messages()) != 2*3 || conversation.currentSummary() != "" {
		t.Error("没开摘要：掉出窗口的直接丢")
	}
}

// 摘要写到一半到了输出上限：半份摘要不用，旧摘要（这里是没有）和没压进去的对话照旧带着。
func TestTruncatedSummaryIsNotUsed(t *testing.T) {
	book := NewConversationBook(nil, Options{MaxHistoryTurn: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	model := &summaryModel{reply: "主人的车停在", truncated: true}
	book.SummarizeWith(Agent{Name: "摘要", Model: model, Instructions: "写摘要。"})
	conversation := book.Conversation("主人")
	for _, text := range []string{"车停在 B2", "第二句", "第三句", "第四句", "第五句"} {
		conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: text},
			{Role: RoleAssistant, Text: "好的。"}}}, 0)
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
	conversation.appendTurn(turn("第一句"), 100)
	conversation.appendTurn(turn("第二句"), 100)
	conversation.appendTurn(turn("第三句"), 1000) // 到了门槛：移出 1 轮（不到一批 3 轮）
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

// sequenceModel 依次回 replies 里的话（每次请求一条），记下每次请求的最后一条消息。
type sequenceModel struct {
	replies []string
	asked   []string
}

func (m *sequenceModel) StreamReply(_ context.Context, request ChatRequest) iter.Seq2[ReplyDelta, error] {
	m.asked = append(m.asked, request.Messages[len(request.Messages)-1].Text)
	reply := m.replies[min(len(m.asked), len(m.replies))-1]
	return func(yield func(ReplyDelta, error) bool) { yield(ReplyDelta{Text: reply, Stop: StopCompleted}, nil) }
}

// 摘要写得太长：不截断（会截在句子中间），请它压短一次，用压短后的那份；压短后还太长就不用（留着旧摘要下次再压）。
func TestOverlongSummaryIsShortenedNotCut(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	turns := []ConversationTurn{{Messages: []ChatMessage{{Role: RoleUser, Text: "车停在 B2"}, {Role: RoleAssistant, Text: "好的。"}}}}
	long := strings.Repeat("长", summaryChars*summaryHardFactor+1)
	model := &sequenceModel{replies: []string{long, "主人的车停在 B2。"}}
	got, err := summarizeTurns(context.Background(), logger, Agent{Name: "摘要", Model: model, Instructions: "写摘要。"}, "", turns)
	if err != nil || got != "主人的车停在 B2。" || len(model.asked) != 2 {
		t.Errorf("应请它压短一次并用压短后的：%q err=%v 请求 %d 次", got, err, len(model.asked))
	}
	model = &sequenceModel{replies: []string{long, long}}
	if got, err := summarizeTurns(context.Background(), logger, Agent{Name: "摘要", Model: model, Instructions: "写摘要。"}, "", turns); err == nil {
		t.Errorf("压短后还太长不该用：%d 字", len([]rune(got)))
	}
}

// failingSummaryModel 写摘要一律出错（拒答、出错都一样：这一批没压进去）。
type failingSummaryModel struct{}

func (failingSummaryModel) StreamReply(context.Context, ChatRequest) iter.Seq2[ReplyDelta, error] {
	return func(yield func(ReplyDelta, error) bool) { yield(ReplyDelta{}, errors.New("拒绝写摘要")) }
}

// unsummarizedSeqs 是攒着等压缩的各轮的编号。
func unsummarizedSeqs(c *Conversation) []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	seqs := make([]int, 0, len(c.unsummarized))
	for _, turn := range c.unsummarized {
		seqs = append(seqs, turn.Seq)
	}
	return seqs
}

// seqRange 是 from..to 的编号。
func seqRange(from, to int) []int {
	var seqs []int
	for seq := from; seq <= to; seq++ {
		seqs = append(seqs, seq)
	}
	return seqs
}

// 摘要一直写不成：攒着等压缩的最多留 maxUnsummarizedTurns 轮（丢最早的），每轮带给模型的历史不再越来越长。
func TestUnsummarizedTurnsAreCapped(t *testing.T) {
	book := NewConversationBook(nil, Options{MaxHistoryTurn: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	book.SummarizeWith(Agent{Name: "摘要", Model: failingSummaryModel{}, Instructions: "写摘要。"})
	conversation := book.Conversation("主人")
	for range 30 {
		conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "问"}, {Role: RoleAssistant, Text: "答"}}}, 0)
	}
	if got := unsummarizedSeqs(conversation); !slices.Equal(got, seqRange(28-maxUnsummarizedTurns+1, 28)) {
		t.Errorf("攒着的只留最近 %d 轮（第 29、30 轮在窗口里）：%v", maxUnsummarizedTurns, got)
	}
	if got := len(conversation.messages()); got != (maxUnsummarizedTurns+2)*3 { // 每轮：时间标记 + 两条
		t.Errorf("每轮带给模型的历史有上限：%d 条", got)
	}
}

// 存着的对话里攒着的轮数超过上限（加上限之前存下的）：读出来时先不截（还没编号的要先编号、写进完整的对话记录），
// 记下一轮时按上限截，并写盘。
func TestStoredUnsummarizedTurnsAreCappedOnNextTurn(t *testing.T) {
	var stored []ConversationTurn
	for seq := 1; seq <= maxUnsummarizedTurns+5; seq++ {
		stored = append(stored, ConversationTurn{Seq: seq, Messages: []ChatMessage{{Role: RoleUser, Text: "问"}}})
	}
	store := &memoryStore{records: map[string]ConversationRecord{"主人": {Person: "主人", TurnCount: len(stored), Unsummarized: stored}}}
	book := NewConversationBook(store, Options{MaxHistoryTurn: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	book.SummarizeWith(Agent{Name: "摘要", Model: failingSummaryModel{}, Instructions: "写摘要。"})
	conversation := book.Conversation("主人")
	conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "新的"}}}, 0)
	if got := unsummarizedSeqs(conversation); !slices.Equal(got, seqRange(6, maxUnsummarizedTurns+5)) {
		t.Errorf("记下一轮时按上限截：%v", got)
	}
	if saved, _, _ := store.LoadConversation("主人"); len(saved.Unsummarized) != maxUnsummarizedTurns {
		t.Errorf("截了要写盘：%d 轮", len(saved.Unsummarized))
	}
}

// gatedSummaryModel 写摘要前先报一声（entered），等测试放行（release）再交出摘要。
type gatedSummaryModel struct {
	entered chan struct{}
	release chan struct{}
}

func (m *gatedSummaryModel) StreamReply(ctx context.Context, _ ChatRequest) iter.Seq2[ReplyDelta, error] {
	return func(yield func(ReplyDelta, error) bool) {
		m.entered <- struct{}{}
		select {
		case <-m.release:
			yield(ReplyDelta{Text: "聊过前三轮。", Stop: StopCompleted}, nil)
		case <-ctx.Done():
			yield(ReplyDelta{}, ctx.Err())
		}
	}
}

// summarySavingStore 存下有摘要的那份时报一声（压缩写成后的那次写盘）。
type summarySavingStore struct {
	memoryStore
	summarized chan struct{}
}

func (s *summarySavingStore) SaveConversation(record ConversationRecord) error {
	err := s.memoryStore.SaveConversation(record)
	if record.Summary != "" {
		s.summarized <- struct{}{}
	}
	return err
}

// 压缩期间攒着的超过上限、最早的几轮被丢掉：压完按编号去掉压进去的那几轮，不按位置（按位置会把没压过的几轮也去掉）。
func TestSummaryRemovesSummarizedTurnsBySeq(t *testing.T) {
	store := &summarySavingStore{summarized: make(chan struct{}, 1)}
	book := NewConversationBook(store, Options{MaxHistoryTurn: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	model := &gatedSummaryModel{entered: make(chan struct{}, 1), release: make(chan struct{})}
	book.SummarizeWith(Agent{Name: "摘要", Model: model, Instructions: "写摘要。"})
	conversation := book.Conversation("主人")
	turn := ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "问"}, {Role: RoleAssistant, Text: "答"}}}
	for range 5 { // 第 1–3 轮掉出窗口：够一批，开始压（卡在模型那里）
		conversation.appendTurn(turn, 0)
	}
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("够一批了，应该开始压缩")
	}
	for range 13 { // 压缩期间又掉出第 4–16 轮：攒着的超过上限，最早的第 1–4 轮被丢掉
		conversation.appendTurn(turn, 0)
	}
	close(model.release)
	select {
	case <-store.summarized:
	case <-time.After(5 * time.Second):
		t.Fatal("摘要写成后应写盘")
	}
	if got := unsummarizedSeqs(conversation); !slices.Equal(got, seqRange(5, 16)) {
		t.Errorf("压进去的是第 1–3 轮（已经被丢掉）：没压过的第 5–16 轮都要留着：%v", got)
	}
}
