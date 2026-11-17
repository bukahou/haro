package brain

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// 对话跟着人走（v4 D1）：历史、当前性格、等确认的操作属于「人」，不属于某一条终端连接。
// 换终端、断线重连、服务端重启，只要声纹认出是同一个人，接着的是同一段对话。
//
//	ConversationBook（服务端进程里一份，所有终端连接共用）
//	  └ Conversation（每人一份；没开声纹时全家一份 SharedPerson）
//	        ↕ ConversationStore（存盘的接口：第一版存成文件，以后可换 Redis 等）

// SharedPerson 是没开声纹（或这一轮没比对）时用的对话。声纹认不出的人不回答，不会建对话。
const SharedPerson = "_shared"

// ConversationRecord 是存盘的一份对话。等确认的操作只在内存里（只保留一轮，重启就作废）。
type ConversationRecord struct {
	Person        string          `json:"person"`
	PersonaID     string          `json:"persona_id,omitempty"`
	History       [][]ChatMessage `json:"history"`
	Summary       string          `json:"summary,omitempty"`      // 滚动摘要（v5 R4）：掉出窗口的对话压成的一段话
	Unsummarized  [][]ChatMessage `json:"unsummarized,omitempty"` // 已经掉出窗口、还没压进摘要的几轮
	ToolCallCount int             `json:"tool_call_count"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// ConversationStore 存取对话（Go interface，实现在 internal/conversationstore）。
type ConversationStore interface {
	// LoadConversation 读一个人的对话；没存过时返回 ok=false。
	LoadConversation(person string) (record ConversationRecord, ok bool, err error)
	SaveConversation(record ConversationRecord) error
}

// ConversationBook 管所有人的对话：第一次用到时从存储读出来，之后留在内存里；每轮结束写盘。
type ConversationBook struct {
	store    ConversationStore // nil = 只在内存里（测试 / 没配目录）
	opts     Options
	logger   *slog.Logger
	mu       sync.Mutex
	byPerson map[string]*Conversation

	summaryModel ChatModel // 滚动摘要用的模型（SummarizeWith）；nil = 不写摘要
}

// NewConversationBook 创建对话簿。opts 提供性格档案、默认性格与历史轮数。
func NewConversationBook(store ConversationStore, opts Options, logger *slog.Logger) *ConversationBook {
	return &ConversationBook{store: store, opts: opts, logger: logger, byPerson: map[string]*Conversation{}}
}

// Conversation 返回这个人的对话（没有就建一个：先读存储，读不到就用初始性格开始）。
func (b *ConversationBook) Conversation(person string) *Conversation {
	if person == "" {
		person = SharedPerson
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if conversation, ok := b.byPerson[person]; ok {
		return conversation
	}
	conversation := &Conversation{person: person, book: b}
	if b.store != nil {
		record, ok, err := b.store.LoadConversation(person)
		switch {
		case err != nil:
			b.logger.Warn("读取对话失败：从头开始", "person", person, "err", err)
		case ok:
			conversation.history = record.History
			conversation.summary, conversation.unsummarized = record.Summary, record.Unsummarized
			conversation.toolCallCount = record.ToolCallCount
			conversation.persona = findPersona(b.opts.Personas, record.PersonaID)
			b.logger.Info("接上之前的对话", "person", person, "turns", len(record.History),
				"since", record.UpdatedAt.Format(time.DateTime))
		}
	}
	if conversation.persona == nil {
		conversation.persona = b.initialPersona()
	}
	b.byPerson[person] = conversation
	return conversation
}

// initialPersona 是新对话的性格：上次切换过的（全家共用的记录，P-3）→ 默认 → 第一个。
func (b *ConversationBook) initialPersona() *Persona {
	persona, err := initialPersona(b.opts)
	if err != nil {
		b.logger.Warn("读取上次的性格失败，用默认性格", "err", err)
		fallback := b.opts
		fallback.PersonaState = nil
		persona, _ = initialPersona(fallback)
	}
	return persona
}

// save 把一份对话写盘（失败只记日志：内存里的对话照常用）。
func (b *ConversationBook) save(conversation *Conversation) {
	if b.store == nil {
		return
	}
	if err := b.store.SaveConversation(conversation.record()); err != nil {
		b.logger.Warn("保存对话失败", "person", conversation.person, "err", err)
	}
}

// Conversation 是一个人的对话。方法都自己加锁，也从不反过来锁 Session（锁顺序只有 Session.mu → Conversation.mu）。
type Conversation struct {
	person string
	book   *ConversationBook

	mu                  sync.Mutex
	history             [][]ChatMessage // 每一轮一组消息：用户的话 + 工具往来 + 最终回答（不含图片）
	persona             *Persona        // 当前性格（没有性格档案时为 nil）
	pendingConfirmation []ToolCall      // 上一轮问了「确定要删掉吗」、等回答的操作（只保留一轮）
	toolCallCount       int             // 用来生成对话内唯一的工具调用 ID
	summary             string          // 滚动摘要（v5 R4）
	unsummarized        [][]ChatMessage // 掉出窗口、还没压进摘要的几轮（照常带给模型，压完再去掉）
	summarizing         bool            // 后台正在压缩
}

// Person 是这份对话属于谁。
func (c *Conversation) Person() string { return c.person }

func (c *Conversation) record() ConversationRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	record := ConversationRecord{Person: c.person, History: c.history, ToolCallCount: c.toolCallCount,
		Summary: c.summary, Unsummarized: c.unsummarized, UpdatedAt: time.Now()}
	if c.persona != nil {
		record.PersonaID = c.persona.ID
	}
	return record
}

// messages 返回历史消息的副本（不含系统提示词）。
func (c *Conversation) messages() []ChatMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	var messages []ChatMessage
	for _, turn := range c.unsummarized { // 还没压进摘要的，先照常带着
		messages = append(messages, turn...)
	}
	for _, turn := range c.history {
		messages = append(messages, turn...)
	}
	return messages
}

// currentSummary 是滚动摘要（没有时为空）。
func (c *Conversation) currentSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.summary
}

// appendTurn 记下一轮（只留最近 maxTurns 轮）并写盘。
func (c *Conversation) appendTurn(turn []ChatMessage, maxTurns int) {
	summarizing := c.book.summarizer() != nil // 先取：不在持有 Conversation.mu 时去锁对话簿
	c.mu.Lock()
	c.history = append(c.history, turn)
	if maxTurns > 0 && len(c.history) > maxTurns {
		dropped := c.history[:len(c.history)-maxTurns]
		c.history = c.history[len(c.history)-maxTurns:]
		if summarizing { // 写摘要时才攒（否则和以前一样直接丢掉）
			c.unsummarized = append(c.unsummarized, dropped...)
		}
	}
	if !summarizing {
		c.unsummarized = nil // 关掉摘要后，以前攒下的不再带
	}
	c.mu.Unlock()
	c.book.save(c)
	c.book.maybeSummarize(c)
}

// replaceLastAnswer 把最近一轮的回答改成 text（被打断时只记听到的部分）并写盘；返回改没改。
func (c *Conversation) replaceLastAnswer(text string) bool {
	c.mu.Lock()
	changed := false
	if n := len(c.history); n > 0 {
		turn := c.history[n-1]
		if last := len(turn) - 1; last >= 0 && turn[last].Role == RoleAssistant {
			turn[last].Text = text
			changed = true
		}
	}
	c.mu.Unlock()
	if changed {
		c.book.save(c)
	}
	return changed
}

func (c *Conversation) currentPersona() *Persona {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persona
}

// setPersona 换性格并写盘。
func (c *Conversation) setPersona(persona *Persona) {
	c.mu.Lock()
	c.persona = persona
	c.mu.Unlock()
	c.book.save(c)
}

// nextToolCallID 生成对话内唯一的工具调用 ID（存盘后重启也不重复）。
func (c *Conversation) nextToolCallID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolCallCount++
	return fmt.Sprintf("call_%d", c.toolCallCount)
}

func (c *Conversation) takePendingConfirmation() []ToolCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending := c.pendingConfirmation
	c.pendingConfirmation = nil
	return pending
}

func (c *Conversation) setPendingConfirmation(calls []ToolCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingConfirmation = calls
}
