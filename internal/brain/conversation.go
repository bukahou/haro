package brain

import (
	"context"
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

// VoiceThread 是语音那条对话（每人一条，终端上说的都接在这里）。一个人还可以有别的线程（App 里新建的对话），
// 轮次编号是这个人所有线程共用的一套（完整的对话记录、长期记忆的索引都按（人, 编号）找）。
const VoiceThread = ""

// ConversationRecord 是存盘的一份对话。等确认的操作只在内存里（只保留一轮，重启就作废）。
type ConversationRecord struct {
	Person        string
	Thread        string // 哪一条对话：VoiceThread（""）= 语音那条；App 里新建的各是一个编号
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
	// LoadConversation 读一个人的一条对话；没存过时返回 ok=false，读出错时返回 err（和「没存过」分开）。
	LoadConversation(person, thread string) (record ConversationRecord, ok bool, err error)
	// LastSeq 是这个人用过的最大轮次编号（所有线程一起算；没有时为 0）。
	LastSeq(person string) (int, error)
	// SaveConversation 整份存下一个人的对话，并把编了号的轮次写进完整的对话记录（只增不删）：
	// 记录里同一编号已经是另一轮时不覆盖它，照样存下这份对话，返回说明哪几轮没写进记录的错误。
	SaveConversation(record ConversationRecord) error
}

// ConversationBook 管所有人的对话：第一次用到时从存储读出来，之后留在内存里；每轮结束写盘。
type ConversationBook struct {
	store  ConversationStore // nil = 只在内存里（测试 / 没配目录）
	opts   Options
	logger *slog.Logger
	mu     sync.Mutex
	byKey  map[conversationKey]*Conversation
	seqMu  sync.Mutex     // 发轮次编号（不和 mu 一起锁：给轮次编号时可能正持有 mu 或 Conversation.mu）
	seqs   map[string]int // 人 → 发出去的最大编号（第一次用到时从存储读）

	summaryAgent *Agent // 写滚动摘要的代理（SummarizeWith）；nil = 不写摘要
}

// NewConversationBook 创建对话簿。opts 提供性格档案、默认性格与历史轮数。
func NewConversationBook(store ConversationStore, opts Options, logger *slog.Logger) *ConversationBook {
	return &ConversationBook{store: store, opts: opts, logger: logger, byKey: map[conversationKey]*Conversation{}, seqs: map[string]int{}}
}

// conversationKey 是一条对话：谁的、哪一条。
type conversationKey struct{ person, thread string }

// nextSeq 给这个人发下一个轮次编号（所有线程共用一套）。known 是调用方手里这条对话已经用到的编号（存储读不出时也不会往回发）。
// 存储读出错时按内存里知道的接着发：万一和存着的撞号，完整的对话记录不会被覆盖（见 ConversationStore.SaveConversation）。
func (b *ConversationBook) nextSeq(person string, known int) int {
	b.seqMu.Lock()
	defer b.seqMu.Unlock()
	last, loaded := b.seqs[person]
	if !loaded && b.store != nil {
		stored, err := b.store.LastSeq(person)
		if err != nil {
			b.logger.Warn("读这个人用过的轮次编号出错：按内存里知道的接着编", "person", person, "err", err)
		}
		last = stored
	}
	last = max(last, known) + 1
	b.seqs[person] = last
	return last
}

// Conversation 返回这个人的对话（没有就建一个：先读存储，没存过就用初始性格开始）。
// 存储读出错时不当成「没存过」：交出一份只在内存里、不写盘的对话（见 Conversation.detached），不留在对话簿里，
// 下一轮重读——空对话一旦写盘，就会盖掉存着的摘要和最近几轮。
func (b *ConversationBook) Conversation(person string) *Conversation {
	return b.ConversationIn(person, VoiceThread)
}

// ConversationIn 返回这个人的某一条对话（thread 为 VoiceThread 时就是语音那条，同 Conversation）。
func (b *ConversationBook) ConversationIn(person, thread string) *Conversation {
	if person == "" {
		person = SharedPerson
	}
	key := conversationKey{person: person, thread: thread}
	b.mu.Lock()
	if conversation, ok := b.byKey[key]; ok {
		b.mu.Unlock()
		return conversation
	}
	conversation := &Conversation{person: person, thread: thread, book: b}
	recovered := false // 接上了上次中途重启没收尾的一轮：要写盘清掉那条记录
	if b.store != nil {
		record, ok, err := b.store.LoadConversation(person, thread)
		switch {
		case err != nil:
			b.mu.Unlock()
			b.logger.Warn("读取对话失败：这一轮不带之前的对话、也不写盘（存着的不会被盖掉），下一轮重读", "person", person, "err", err)
			conversation.detached = true
			conversation.persona = b.initialPersona()
			return conversation
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
				recovered = true
			}
			// 还没编号的（旧的记录）补上，写盘后进完整的对话记录。窗口和攒着等压缩的轮数等记下一轮时再按上限截
			// （appendTurn）：截掉之前它们都已经编号、写进了完整的对话记录
			if conversation.numberTurnsLocked() {
				recovered = true
			}
		}
	}
	if conversation.persona == nil {
		conversation.persona = b.initialPersona()
	}
	b.byKey[key] = conversation
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

// save 把一份对话写盘（出错只记日志：内存里的对话照常用）；不写盘的对话（detached）不写。
// 同一个人的保存一个接一个：在轮到时才取快照，后取的快照一定后写，旧的不会盖掉新的（摘要和新一轮同时保存时）。
func (b *ConversationBook) save(conversation *Conversation) {
	if b.store == nil || conversation.detached {
		return
	}
	conversation.saveMu.Lock()
	defer conversation.saveMu.Unlock()
	if err := b.store.SaveConversation(conversation.record()); err != nil {
		b.logger.Warn("保存对话出错", "person", conversation.person, "err", err)
	}
}

// Conversation 是一个人的一条对话。方法都自己加锁，也从不反过来锁 Session（锁顺序只有 Session.mu → Conversation.mu）。
type Conversation struct {
	person string
	thread string // VoiceThread = 语音那条
	book   *ConversationBook
	// detached：存储读这个人的对话时出错，这一份只在内存里用：不带之前的对话、不写盘、不压缩，也不留在对话簿里
	// （下一轮重读）。建好后不再改，不用加锁
	detached bool

	saveMu sync.Mutex // 同一个人的保存一个接一个（见 ConversationBook.save）

	mu             sync.Mutex
	history        []ConversationTurn // 每一轮：说话的时间 + 用户的话、工具往来、最终回答（不含图片）
	persona        *Persona           // 当前性格（没有性格档案时为 nil）
	pendingActions []pendingAction    // 等主人确认的操作（确认闸，见 confirmation.go）
	actionCount    int                // 待确认操作编到第几个了
	toolCallCount  int                // 用来生成对话内唯一的工具调用 ID
	turnCount      int                // 这条对话最近一轮的编号（编号是这个人所有线程共用的一套，见 ConversationBook.nextSeq）
	summary        string             // 滚动摘要（v5 R4）
	unsummarized   []ConversationTurn // 掉出窗口、还没压进摘要的几轮（照常带给模型，压完再去掉）
	summarizing    bool               // 后台正在压缩
	compacted      bool               // 上下文太长、提前移出了窗口：不等攒够一批就压缩
	openTurn       *ConversationTurn  // 还没收尾的一轮（见 ConversationRecord.OpenTurn）
}

// Person 是这份对话属于谁。
func (c *Conversation) Person() string { return c.person }

// Thread 是哪一条对话（VoiceThread = 语音那条）。
func (c *Conversation) Thread() string { return c.thread }

// record 取一份写盘用的快照。轮次和消息都复制一份：写盘在锁外进行，期间这份对话还会被改（被打断时改回答文字），
// 快照不能和它共用底层数组。消息里的 ToolCalls、Image 只会整体替换、不会原地改，不必再往下复制。
func (c *Conversation) record() ConversationRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	record := ConversationRecord{Person: c.person, Thread: c.thread, History: cloneTurns(c.history), ToolCallCount: c.toolCallCount,
		TurnCount: c.turnCount, Summary: c.summary, Unsummarized: cloneTurns(c.unsummarized), UpdatedAt: time.Now()}
	if c.openTurn != nil {
		open := cloneTurns([]ConversationTurn{*c.openTurn})[0]
		record.OpenTurn = &open
	}
	if c.persona != nil {
		record.PersonaID = c.persona.ID
	}
	return record
}

// cloneTurns 复制轮次和每一轮的消息切片。
func cloneTurns(turns []ConversationTurn) []ConversationTurn {
	if turns == nil {
		return nil
	}
	cloned := make([]ConversationTurn, len(turns))
	for i, turn := range turns {
		turn.Messages = slices.Clone(turn.Messages)
		cloned[i] = turn
	}
	return cloned
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

// turnTimeLabel 是一轮前面的时间标记（本地时间，和每轮告诉模型的「现在是……」同一种写法、同一个时区：年份和星期都带上，
// 历史跨好几天时分得清「上周三说的」）；不知道时间的写明不知道。
func turnTimeLabel(at time.Time) string {
	if at.IsZero() {
		return "〔这一轮的时间不详〕"
	}
	at = at.Local()
	return "〔" + DateText(at) + at.Format("15:04") + "〕"
}

// currentSummary 是滚动摘要（没有时为空）。
func (c *Conversation) currentSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.summary
}

// compactKeepTurns：上下文太长时，提前移出窗口后留下最近几轮。
const compactKeepTurns = 2

// unknownUsageTurns：不按轮数截（MaxHistoryTurn 为 0）而模型服务不报用量时，改按这么多轮截：按 token 压缩对它不起作用，
// 不截的话历史会一直变长。用的是不限轮数之前的默认窗口。
const unknownUsageTurns = 6

// appendTurn 记下一轮（编上号，只留最近 maxTurns 轮）并写盘；这一轮收尾了，「没收尾」的记录随之清掉。
// contextTokens 是这一轮请求模型时带的上下文（最大的那次请求的输入 token，模型报的）：到了 Options.CompactContextTokens，
// 最近 compactKeepTurns 轮之前的提前移出窗口、马上压缩（完整的对话记录里都还在）。contextUnknown：这一轮的用量没报，
// 不按轮数截时改按 unknownUsageTurns 截。
func (c *Conversation) appendTurn(turn ConversationTurn, contextTokens int, contextUnknown bool) {
	summarizing := c.book.summarizer() != nil // 先取：不在持有 Conversation.mu 时去锁对话簿
	c.mu.Lock()
	c.turnCount = c.nextSeqLocked()
	turn.Seq = c.turnCount
	c.history = append(c.history, turn)
	c.openTurn = nil
	c.trimLocked(c.book.opts.MaxHistoryTurn, summarizing) // 窗口轮数只由对话簿持有（和重启时接上用的是同一个数）
	if c.book.opts.MaxHistoryTurn == 0 && contextUnknown && len(c.history) > unknownUsageTurns {
		c.book.logger.Info("模型服务没报用量：按 token 压缩不起作用，这一轮改按轮数截", "person", c.person, "keep_turns", unknownUsageTurns)
		c.trimLocked(unknownUsageTurns, summarizing)
	}
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

// idleCheckEvery 是多久看一次有没有闲着的对话（RunIdleCompaction）：比闲置的门槛短得多就行，看一遍只是遍历内存里的几份对话。
const idleCheckEvery = time.Minute

// RunIdleCompaction 在后台定时把闲了 Options.IdleCompactAfter 以上的对话提前压缩，直到 ctx 结束；门槛是 0 时直接返回。
// 隔了很久再开口时，第一次请求不必带着整段旧对话：提示词缓存这时多半已经过期，整段要按没缓存的价格重新处理、开口也慢。
func (b *ConversationBook) RunIdleCompaction(ctx context.Context) {
	idle := b.opts.IdleCompactAfter
	if idle <= 0 {
		return
	}
	ticker := time.NewTicker(min(idleCheckEvery, idle))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			b.compactIdle(now)
		}
	}
}

// compactIdle 把到 now 为止闲了 Options.IdleCompactAfter 以上的对话提前压缩（压过的不再重复压）。
func (b *ConversationBook) compactIdle(now time.Time) {
	summarizing := b.summarizer() != nil
	b.mu.Lock()
	conversations := make([]*Conversation, 0, len(b.byKey))
	for _, conversation := range b.byKey {
		conversations = append(conversations, conversation)
	}
	b.mu.Unlock()
	for _, conversation := range conversations {
		if conversation.compactIfIdle(now, b.opts.IdleCompactAfter, summarizing) {
			b.save(conversation)
			b.maybeSummarize(conversation)
		}
	}
}

// compactIfIdle 在最后一轮之后闲了 idle 以上时，把最近 compactKeepTurns 轮之前的移出窗口（和上下文太长时一样），返回移没移。
// 正在进行的一轮（有没收尾的记录）、不知道时间的旧记录、本来就没几轮的不动。
func (c *Conversation) compactIfIdle(now time.Time, idle time.Duration, summarizing bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.detached || c.openTurn != nil || len(c.history) <= compactKeepTurns {
		return false
	}
	last := c.history[len(c.history)-1].At
	if last.IsZero() || now.Sub(last) < idle {
		return false
	}
	c.book.logger.Info("对话闲了一阵：提前移出窗口、压缩", "person", c.person, "idle", now.Sub(last).Round(time.Minute),
		"moved_turns", len(c.history)-compactKeepTurns)
	c.trimLocked(compactKeepTurns, summarizing)
	c.compacted = summarizing
	return true
}

// lastTurnAt 是最近一轮的时间（还没压进摘要的也算）；不知道或一轮都没有时是零值。
func (c *Conversation) lastTurnAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, turns := range [][]ConversationTurn{c.history, c.unsummarized} {
		if len(turns) > 0 {
			return turns[len(turns)-1].At
		}
	}
	return time.Time{}
}

// nextSeqLocked 是这条对话下一轮的编号：这个人所有线程共用的一套（对话簿发号）；不写盘的那份（detached）
// 只在自己手里往下数，不占共用的号（它的轮次不会存下来）。
func (c *Conversation) nextSeqLocked() int {
	if c.detached {
		return c.turnCount + 1
	}
	return c.book.nextSeq(c.person, c.turnCount)
}

// numberTurnsLocked 给还没编号的轮次补上号（按先后：掉出窗口的在前），返回补没补。调用方持有锁（或对话还没交出去）。
func (c *Conversation) numberTurnsLocked() bool {
	numbered := false
	for _, turns := range [][]ConversationTurn{c.unsummarized, c.history} {
		for i := range turns {
			if turns[i].Seq == 0 {
				c.turnCount = c.nextSeqLocked()
				turns[i].Seq, numbered = c.turnCount, true
			}
		}
	}
	return numbered
}

// maxUnsummarizedTurns 是掉出窗口、还没压进摘要的轮次最多攒几轮（它们照常带给模型）。正常时压一次只要几秒，
// 攒着的不会超过一批多一点；攒到四批说明摘要连着几次没写成（拒答、写得太长、出错），再攒下去每轮带给模型的历史只会越来越长
// （费用和开口前的等待跟着涨，最后超出模型的上下文）。超出的丢掉最早的几轮：完整的对话记录里都还在。
const maxUnsummarizedTurns = 4 * summaryBatch

// trimLocked 只留最近 maxTurns 轮；掉出窗口的在写摘要时攒着等压缩（最多 maxUnsummarizedTurns 轮），否则丢掉。
// 调用方持有 c.mu。
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
		return
	}
	if excess := len(c.unsummarized) - maxUnsummarizedTurns; excess > 0 {
		c.book.logger.Warn("摘要一直没写成：攒着的最早几轮不再带给模型（完整的对话记录里都有）", "person", c.person,
			"dropped_turns", excess, "kept_turns", maxUnsummarizedTurns)
		c.unsummarized = slices.Clone(c.unsummarized[excess:])
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

// replaceLastAnswer 把内存里最近一轮的回答改成 text（被打断时只记听到的部分），返回改没改（本来就是 text 时不改）。
// 不写盘：调用方常持有 Session.mu，写盘要等别的保存、可能慢，改完后由调用方放开锁再调 persist。
func (c *Conversation) replaceLastAnswer(text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.history); n > 0 {
		turn := c.history[n-1].Messages
		if last := len(turn) - 1; last >= 0 && turn[last].Role == RoleAssistant && turn[last].Text != text {
			turn[last].Text = text
			return true
		}
	}
	return false
}

// persist 把这份对话写盘（见 ConversationBook.save）。
func (c *Conversation) persist() { c.book.save(c) }

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
