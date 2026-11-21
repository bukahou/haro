package brain

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// 对话跟着人走：历史、当前性格、等确认的操作属于「人」，不属于某一条终端连接。
// 换终端、断线重连、服务端重启，只要声纹认出是同一个人，接着的是同一段对话。
//
//	ConversationBook（服务端进程里一份，所有终端连接共用）
//	  └ Conversation（每人一份；没开声纹时全家一份 SharedPerson）
//	        ↕ ConversationStore（存盘的接口，现在存在 SQLite）

// SharedPerson 是没开声纹（或这一轮没比对）时用的对话。声纹认不出的人不回答，不会建对话。
const SharedPerson = "_shared"

// ConversationRecord 是存盘的一份对话。等确认的操作只在内存里（只保留一轮，重启就作废）。
type ConversationRecord struct {
	Person        string
	PersonaID     string
	History       []ConversationTurn // 最近几轮
	Summary       string             // 滚动摘要：掉出窗口的对话压成的一段话
	Unsummarized  []ConversationTurn // 已经掉出窗口、还没压进摘要的几轮
	ToolCallCount int
	TurnCount     int // 轮次编到第几了（每一轮按它编号，完整的对话记录按编号存）
	// OpenTurn 是还没收尾的一轮（做完了工具、还没说完）：已经整理成「服务端重启、没说完」的样子。
	// 正常收尾时清掉；读到它说明上次服务端在这一轮中途重启，照实接进历史
	OpenTurn  *ConversationTurn
	UpdatedAt time.Time
}

// ConversationTurn 是历史里的一轮：主人说话的时间、这一轮的消息（主人的话、工具往来、回答）。
type ConversationTurn struct {
	At       time.Time     `json:"at,omitzero"` // 零值 = 不知道（开始记时间之前的旧记录）
	Messages []ChatMessage `json:"messages"`
	Seq      int           `json:"seq,omitempty"` // 这个人的第几轮（从 1 起；0 = 还没编号）
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

	summaryAgent *Agent // 写滚动摘要的代理（SummarizeWith）；nil = 不写摘要
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
	if conversation, ok := b.byPerson[person]; ok {
		b.mu.Unlock()
		return conversation
	}
	conversation := &Conversation{person: person, book: b}
	recovered := false // 接上了上次中途重启没收尾的一轮：要写盘清掉那条记录
	if b.store != nil {
		record, ok, err := b.store.LoadConversation(person)
		switch {
		case err != nil:
			b.logger.Warn("读取对话失败：从头开始", "person", person, "err", err)
		case ok:
			conversation.history = record.History
			conversation.summary, conversation.unsummarized = record.Summary, record.Unsummarized
			conversation.toolCallCount, conversation.turnCount = record.ToolCallCount, record.TurnCount
			conversation.persona = findPersona(b.opts.Personas, record.PersonaID)
			b.logger.Info("接上之前的对话", "person", person, "turns", len(record.History),
				"since", record.UpdatedAt.Format(time.DateTime))
			if record.OpenTurn != nil { // 上次在这一轮中途重启：做过的工具照实接进历史（模型才知道自己做过）
				b.logger.Warn("上次服务端在一轮中途重启：这一轮照实接进历史", "person", person, "at", record.OpenTurn.At)
				conversation.history = append(conversation.history, *record.OpenTurn)
				conversation.trimLocked(b.opts.MaxHistoryTurn, b.summaryAgent != nil) // 持有 b.mu：直接读，不经 summarizer()
				recovered = true
			}
			if conversation.numberTurnsLocked() { // 还没编号的（旧的记录）补上，写盘后进完整的对话记录
				recovered = true
			}
		}
	}
	if conversation.persona == nil {
		conversation.persona = b.initialPersona()
	}
	b.byPerson[person] = conversation
	b.mu.Unlock()
	if recovered {
		b.save(conversation)
	}
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
// 同一个人的保存一个接一个：在轮到时才取快照，后取的快照一定后写，旧的不会盖掉新的（摘要和新一轮同时保存时）。
func (b *ConversationBook) save(conversation *Conversation) {
	if b.store == nil {
		return
	}
	conversation.saveMu.Lock()
	defer conversation.saveMu.Unlock()
	if err := b.store.SaveConversation(conversation.record()); err != nil {
		b.logger.Warn("保存对话失败", "person", conversation.person, "err", err)
	}
}

// Conversation 是一个人的对话。方法都自己加锁，也从不反过来锁 Session（锁顺序只有 Session.mu → Conversation.mu）。
type Conversation struct {
	person string
	book   *ConversationBook

	saveMu sync.Mutex // 同一个人的保存一个接一个（见 ConversationBook.save）

	mu             sync.Mutex
	history        []ConversationTurn // 每一轮：说话的时间 + 用户的话、工具往来、最终回答（不含图片）
	persona        *Persona           // 当前性格（没有性格档案时为 nil）
	pendingActions []pendingAction    // 等主人确认的操作（确认闸，见 confirmation.go）
	actionCount    int                // 待确认操作编到第几个了
	toolCallCount  int                // 用来生成对话内唯一的工具调用 ID
	turnCount      int                // 轮次编到第几了（见 ConversationTurn.Seq）
	summary        string             // 滚动摘要（v5 R4）
	unsummarized   []ConversationTurn // 掉出窗口、还没压进摘要的几轮（照常带给模型，压完再去掉）
	summarizing    bool               // 后台正在压缩
	compacted      bool               // 上下文太长、提前移出了窗口：不等攒够一批就压缩
	openTurn       *ConversationTurn  // 还没收尾的一轮（见 ConversationRecord.OpenTurn）
}

// Person 是这份对话属于谁。
func (c *Conversation) Person() string { return c.person }

func (c *Conversation) record() ConversationRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	record := ConversationRecord{Person: c.person, History: c.history, ToolCallCount: c.toolCallCount, TurnCount: c.turnCount,
		Summary: c.summary, Unsummarized: c.unsummarized, OpenTurn: c.openTurn, UpdatedAt: time.Now()}
	if c.persona != nil {
		record.PersonaID = c.persona.ID
	}
	return record
}

// messages 返回历史消息的副本（不含系统提示词）。每一轮前面加一条时间标记（系统合成）：
// 模型分得清哪句是什么时候说的（隔天的「明天」、几十分钟前说的「现在几点」）；标记由存下的时间算出，每次都一样，不影响提示词缓存。
func (c *Conversation) messages() []ChatMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	var messages []ChatMessage
	for _, turns := range [][]ConversationTurn{c.unsummarized, c.history} { // 还没压进摘要的，先照常带着
		for _, turn := range turns {
			messages = append(messages, ChatMessage{Role: RoleUser, Text: turnTimeLabel(turn.At), Synthetic: true})
			messages = append(messages, turn.Messages...)
		}
	}
	return messages
}

// turnTimeLabel 是一轮前面的时间标记（本地时间，和每轮告诉模型的「现在」同一个时区）；不知道时间的写明不知道。
func turnTimeLabel(at time.Time) string {
	if at.IsZero() {
		return "（这一轮的时间不详）"
	}
	at = at.Local()
	return fmt.Sprintf("（%d 月 %d 日 %s）", at.Month(), at.Day(), at.Format("15:04"))
}

// currentSummary 是滚动摘要（没有时为空）。
func (c *Conversation) currentSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.summary
}

// compactKeepTurns：上下文太长时，提前移出窗口后留下最近几轮。
const compactKeepTurns = 2

// appendTurn 记下一轮（编上号，只留最近 maxTurns 轮）并写盘；这一轮收尾了，「没收尾」的记录随之清掉。
// contextTokens 是这一轮请求模型时带的上下文（最大的那次请求的输入 token，模型报的）：到了 Options.CompactContextTokens，
// 最近 compactKeepTurns 轮之前的提前移出窗口、马上压缩（完整的对话记录里都还在）。
func (c *Conversation) appendTurn(turn ConversationTurn, maxTurns, contextTokens int) {
	summarizing := c.book.summarizer() != nil // 先取：不在持有 Conversation.mu 时去锁对话簿
	c.mu.Lock()
	c.turnCount++
	turn.Seq = c.turnCount
	c.history = append(c.history, turn)
	c.openTurn = nil
	c.trimLocked(maxTurns, summarizing)
	if threshold := c.book.opts.CompactContextTokens; threshold > 0 && contextTokens >= threshold && len(c.history) > compactKeepTurns {
		c.book.logger.Info("上下文太长：提前移出窗口、压缩", "person", c.person, "context_tokens", contextTokens,
			"threshold", threshold, "moved_turns", len(c.history)-compactKeepTurns)
		c.trimLocked(compactKeepTurns, summarizing)
		c.compacted = summarizing
	}
	c.mu.Unlock()
	c.book.save(c)
	c.book.maybeSummarize(c)
}

// numberTurnsLocked 给还没编号的轮次补上号（按先后：掉出窗口的在前），返回补没补。调用方持有锁（或对话还没交出去）。
func (c *Conversation) numberTurnsLocked() bool {
	numbered := false
	for _, turns := range [][]ConversationTurn{c.unsummarized, c.history} {
		for i := range turns {
			if turns[i].Seq == 0 {
				c.turnCount++
				turns[i].Seq, numbered = c.turnCount, true
			}
		}
	}
	return numbered
}

// trimLocked 只留最近 maxTurns 轮；掉出窗口的在写摘要时攒着等压缩，否则丢掉。调用方持有 c.mu（或对话还没交出去）。
func (c *Conversation) trimLocked(maxTurns int, summarizing bool) {
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
}

// keepOpenTurn 记下还没收尾的这一轮（已经整理成「服务端重启、没说完」的样子）并写盘：做完了工具时调用，
// 这一轮中途服务端重启的话，下次读到它照实接进历史。
func (c *Conversation) keepOpenTurn(turn ConversationTurn) {
	c.mu.Lock()
	c.openTurn = &turn
	c.mu.Unlock()
	c.book.save(c)
}

// replaceLastAnswer 把最近一轮的回答改成 text（被打断时只记听到的部分）并写盘；返回改没改（本来就是 text 时不改）。
func (c *Conversation) replaceLastAnswer(text string) bool {
	c.mu.Lock()
	changed := false
	if n := len(c.history); n > 0 {
		turn := c.history[n-1].Messages
		if last := len(turn) - 1; last >= 0 && turn[last].Role == RoleAssistant && turn[last].Text != text {
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

// nextActionID 给待确认的操作编号（这个人的对话里从 act_1 数起）。
func (c *Conversation) nextActionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actionCount++
	return fmt.Sprintf("act_%d", c.actionCount)
}

// addPendingAction 存下一条待确认的操作。
func (c *Conversation) addPendingAction(action pendingAction) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingActions = append(c.pendingActions, action)
}

// pendingAction 按编号找待确认的操作（不取走）。
func (c *Conversation) pendingAction(id string) (pendingAction, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, action := range c.pendingActions {
		if action.ID == id {
			return action, true
		}
	}
	return pendingAction{}, false
}

// takePendingAction 按编号取走一条待确认的操作。
func (c *Conversation) takePendingAction(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingActions = slices.DeleteFunc(c.pendingActions, func(action pendingAction) bool { return action.ID == id })
}

// dropPendingActions 作废所有待确认的操作，返回作废了几条。
func (c *Conversation) dropPendingActions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := len(c.pendingActions)
	c.pendingActions = nil
	return dropped
}

// dropPendingActionsFrom 作废在第 turn 轮存下的待确认操作，返回作废了几条。
func (c *Conversation) dropPendingActionsFrom(turn int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := len(c.pendingActions)
	c.pendingActions = slices.DeleteFunc(c.pendingActions, func(action pendingAction) bool { return action.Turn == turn })
	return before - len(c.pendingActions)
}

// unexpiredPendingActions 去掉过期的，返回剩下的待确认操作（副本）。
func (c *Conversation) unexpiredPendingActions(now time.Time) []pendingAction {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingActions = slices.DeleteFunc(c.pendingActions, func(action pendingAction) bool { return now.After(action.ExpiresAt) })
	return slices.Clone(c.pendingActions)
}

// hasPendingActions 去掉过期的，看还有没有待确认的操作。
func (c *Conversation) hasPendingActions(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingActions = slices.DeleteFunc(c.pendingActions, func(action pendingAction) bool { return now.After(action.ExpiresAt) })
	return len(c.pendingActions) > 0
}
