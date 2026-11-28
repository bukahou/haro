package brain

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memoryStore 是测试用的对话存储。
type memoryStore struct {
	mu      sync.Mutex
	records map[string]ConversationRecord
}

func (m *memoryStore) LoadConversation(person, thread string) (ConversationRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[person+"\x00"+thread]
	return record, ok, nil
}

func (m *memoryStore) LastSeq(person string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := 0
	for _, record := range m.records {
		if record.Person == person {
			last = max(last, record.TurnCount)
		}
	}
	return last, nil
}

func (m *memoryStore) SaveConversation(record ConversationRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records == nil {
		m.records = map[string]ConversationRecord{}
	}
	m.records[record.Person+"\x00"+record.Thread] = record
	return nil
}

var bookPersonas = []Persona{{ID: "buddy", Name: "毒舌搭子"}, {ID: "gentle", Name: "温柔陪伴"}}

// v4 D1：每人一份对话（历史、性格各管各的），存盘后新的对话簿（服务端重启）读得回来。
func TestConversationBookSeparatesPeopleAndPersists(t *testing.T) {
	store := &memoryStore{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{Personas: bookPersonas, DefaultPersona: "buddy"}
	book := NewConversationBook(store, opts, logger)

	owner := book.Conversation("主人")
	owner.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "你好"}, {Role: RoleAssistant, Text: "你好呀。"}}}, 0, false)
	owner.setPersona(&bookPersonas[1])
	if id := owner.nextToolCallID(); id != "call_1" {
		t.Errorf("工具调用 ID：%s", id)
	}
	owner.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "再见"}, {Role: RoleAssistant, Text: "拜拜。"}}}, 0, false)
	if book.Conversation("主人") != owner {
		t.Error("同一个人应拿到同一份对话")
	}
	family := book.Conversation("家人")
	if len(family.messages()) != 0 || family.currentPersona().ID != "buddy" {
		t.Errorf("别人的对话应是空的、用默认性格：%v %v", family.messages(), family.currentPersona())
	}

	restarted := NewConversationBook(store, opts, logger).Conversation("主人")
	if got := restarted.messages(); len(got) != 6 || got[5].Text != "拜拜。" { // 每轮：时间标记 + 两条
		t.Errorf("重启后应接上历史：%+v", got)
	}
	if restarted.currentPersona().ID != "gentle" {
		t.Errorf("重启后性格应保持：%v", restarted.currentPersona())
	}
	if id := restarted.nextToolCallID(); id != "call_2" {
		t.Errorf("重启后工具调用 ID 不应重复：%s", id)
	}
}

func TestConversationKeepsRecentTurnsAndReplacesLastAnswer(t *testing.T) {
	book := NewConversationBook(nil, Options{MaxHistoryTurn: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conversation := book.Conversation("")
	if conversation.Person() != SharedPerson {
		t.Errorf("没有名字时用全家的：%q", conversation.Person())
	}
	for _, text := range []string{"一", "二", "三"} {
		conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: text}, {Role: RoleAssistant, Text: text + "。"}}}, 0, false)
	}
	if !conversation.replaceLastAnswer("听到一半") {
		t.Fatal("应能改最近一轮的回答")
	}
	got := conversation.messages()
	if len(got) != 6 || got[1].Text != "二" || got[5].Text != "听到一半" { // 每轮：时间标记 + 两条
		t.Errorf("只留最近 2 轮，最后的回答被改掉：%+v", got)
	}
}

// 每一轮前面带一条时间标记（系统合成）：那一轮主人说话的时间；不知道时间的（旧记录）写明不详。
func TestHistoryTurnsCarryTheirTime(t *testing.T) {
	book := NewConversationBook(nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conversation := book.Conversation("主人")
	at := time.Date(2026, 11, 18, 20, 59, 0, 0, time.Local)
	conversation.appendTurn(ConversationTurn{At: at, Messages: []ChatMessage{{Role: RoleUser, Text: "明天天气怎么样"}, {Role: RoleAssistant, Text: "19 号晴。"}}}, 0, false)
	conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "旧的一轮"}, {Role: RoleAssistant, Text: "好。"}}}, 0, false)

	got := conversation.messages()
	if len(got) != 6 || !got[0].Synthetic || got[0].Role != RoleUser || got[0].Text != turnTimeLabel(at) || got[1].Text != "明天天气怎么样" {
		t.Fatalf("每轮前面应有那一轮的时间标记：%+v", got)
	}
	if want := "〔2026年11月18日（星期三）20:59〕"; got[0].Text != want {
		t.Errorf("时间标记要带年份和星期（和每轮的「现在是……」同一种写法）：%q，应是 %q", got[0].Text, want)
	}
	if !got[3].Synthetic || got[3].Text != turnTimeLabel(time.Time{}) || got[3].Text == got[0].Text {
		t.Errorf("不知道时间的一轮应写明不详：%+v", got[3])
	}
	if again := conversation.messages(); again[0].Text != got[0].Text {
		t.Errorf("同一轮的标记每次都一样（提示词缓存才不受影响）：%q / %q", again[0].Text, got[0].Text)
	}
}

// 一轮做完工具后服务端重启（这一轮没收尾）：下次读这份对话时，这一轮照实接进历史，没收尾的记录随之清掉。
func TestOpenTurnIsRecoveredAfterRestart(t *testing.T) {
	store := &memoryStore{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	conversation := NewConversationBook(store, Options{}, logger).Conversation("主人")
	conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "你好"}, {Role: RoleAssistant, Text: "你好呀。"}}}, 0, false)
	open := ConversationTurn{At: time.Now(), Messages: []ChatMessage{{Role: RoleUser, Text: "记一下车位"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "save_note"}}},
		{Role: RoleTool, ToolName: "save_note", ToolCallID: "call_1", Text: "记好了"},
		{Role: RoleAssistant, Text: restartedUnfinished}}}
	conversation.keepOpenTurn(open) // 做完了工具，还没说完……服务端重启

	restarted := NewConversationBook(store, Options{}, logger).Conversation("主人")
	got := restarted.messages()
	if len(got) != (1+2)+(1+len(open.Messages)) || got[len(got)-2].Text != "记好了" || got[len(got)-1].Text != restartedUnfinished {
		t.Fatalf("中途重启的那一轮应照实接进历史：%+v", got)
	}
	if record, _, _ := store.LoadConversation("主人", ""); record.OpenTurn != nil || len(record.History) != 2 {
		t.Errorf("接上之后应写盘、清掉没收尾的记录：%+v", record)
	}
	// 正常收尾的一轮会替掉没收尾的记录
	restarted.keepOpenTurn(open)
	restarted.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "再见"}, {Role: RoleAssistant, Text: "拜拜。"}}}, 0, false)
	if record, _, _ := store.LoadConversation("主人", ""); record.OpenTurn != nil {
		t.Errorf("正常收尾后不应留着没收尾的记录：%+v", record.OpenTurn)
	}
}

// 每一轮编上号（这个人的第几轮），编到第几存下来；旧的记录里没编号的，读出来时按先后补上并写盘。
func TestTurnsAreNumbered(t *testing.T) {
	store := &memoryStore{records: map[string]ConversationRecord{"主人\x00": {Person: "主人",
		Unsummarized: []ConversationTurn{{Messages: []ChatMessage{{Role: RoleUser, Text: "较早"}}}},
		History:      []ConversationTurn{{Messages: []ChatMessage{{Role: RoleUser, Text: "最近"}}}}}}}
	book := NewConversationBook(store, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conversation := book.Conversation("主人")
	saved := store.records["主人\x00"]
	if saved.TurnCount != 2 || saved.Unsummarized[0].Seq != 1 || saved.History[0].Seq != 2 {
		t.Fatalf("没编号的按先后补上并写盘：%+v", saved)
	}
	conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "新的"}}}, 0, false)
	saved = store.records["主人\x00"]
	if saved.TurnCount != 3 || saved.History[len(saved.History)-1].Seq != 3 {
		t.Errorf("新的一轮接着编号：%+v", saved)
	}
}

// 写盘用的快照和内存里的对话不共用数据：取完快照后再改回答（被打断），快照里还是取的时候的样子（写盘在锁外进行）。
func TestRecordIsIsolatedFromLaterChanges(t *testing.T) {
	book := NewConversationBook(nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conversation := book.Conversation("主人")
	conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "讲个故事"}, {Role: RoleAssistant, Text: "从前有座山。"}}}, 0, false)
	snapshot := conversation.record()
	if !conversation.replaceLastAnswer("从前") {
		t.Fatal("应能改最近一轮的回答")
	}
	turn := snapshot.History[len(snapshot.History)-1].Messages
	if got := turn[len(turn)-1].Text; got != "从前有座山。" {
		t.Errorf("快照被之后的改动改掉了：%q", got)
	}
}

// blockingStore：block 打开后，写盘一直等到 release 关闭，开始写时通知 saving；没打开时照常写。
type blockingStore struct {
	memoryStore
	block   atomic.Bool
	saving  chan struct{}
	release chan struct{}
}

func (b *blockingStore) SaveConversation(record ConversationRecord) error {
	if b.block.Load() {
		b.saving <- struct{}{}
		<-b.release
	}
	return b.memoryStore.SaveConversation(record)
}

// 播放中被打断、回头改历史时，写盘不持有会话锁：写盘慢（等别的保存）时，音频转发等要拿锁的事照常进行。
func TestPlaybackCutSavesWithoutHoldingSessionLock(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &blockingStore{saving: make(chan struct{}, 1), release: make(chan struct{})}
	conversation := NewConversationBook(store, Options{}, logger).Conversation("主人")
	conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "讲个故事"}, {Role: RoleAssistant, Text: "第一句。第二句。"}}}, 0, false)
	store.block.Store(true)

	s := &Session{logger: logger, conversation: conversation, truncateAtMs: -1, answeredInResponse: true,
		spoken: []spokenSegment{{text: "第一句。", startMs: 0, endMs: 1000}, {text: "第二句。", startMs: 1000, endMs: 2000}}}
	done := make(chan struct{})
	go func() { s.truncate(500); close(done) }()
	select {
	case <-store.saving:
	case <-time.After(2 * time.Second):
		t.Fatal("被打断后应写盘")
	}
	locked := make(chan struct{})
	go func() { s.mu.Lock(); s.mu.Unlock(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Error("写盘期间会话锁还被拿着")
	}
	close(store.release)
	<-done
	if got := conversation.messages(); got[len(got)-1].Text != "第一句。" {
		t.Errorf("历史应改成听到的部分：%q", got[len(got)-1].Text)
	}
}

// 不按轮数截（MaxHistoryTurn 为 0）：历史一直留着，只看上下文 token；某一轮的模型服务没报用量时（按 token 压缩对它不起作用），
// 改按 unknownUsageTurns 截，免得历史一直变长。
func TestUnlimitedHistoryFallsBackWhenUsageUnknown(t *testing.T) {
	book := NewConversationBook(nil, Options{CompactContextTokens: 80000}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conversation := book.Conversation("主人")
	turn := func(text string) ConversationTurn {
		return ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: text}, {Role: RoleAssistant, Text: "好。"}}}
	}
	for i := range 30 {
		conversation.appendTurn(turn(fmt.Sprintf("第 %d 句", i+1)), 5000, false)
	}
	if got := len(conversation.messages()) / 3; got != 30 {
		t.Fatalf("不按轮数截、上下文没到门槛：30 轮都该留着，留了 %d 轮", got)
	}
	conversation.appendTurn(turn("第 31 句"), 0, true)
	got := conversation.messages()
	if len(got)/3 != unknownUsageTurns || got[len(got)-2].Text != "第 31 句" {
		t.Errorf("用量没报的一轮：改按 %d 轮截，留最近的：%d 轮，最后一轮 %q", unknownUsageTurns, len(got)/3, got[len(got)-2].Text)
	}
	conversation.appendTurn(turn("第 32 句"), 90000, false)
	if got := len(conversation.messages()) / 3; got != compactKeepTurns {
		t.Errorf("上下文到了门槛：只留最近 %d 轮，留了 %d 轮", compactKeepTurns, got)
	}
}

// 闲了 IdleCompactAfter 以上的对话提前压缩：只留最近 compactKeepTurns 轮；压过的、闲得不够久的、正在进行一轮的、
// 不知道时间的都不动。
func TestIdleConversationsAreCompacted(t *testing.T) {
	book := NewConversationBook(nil, Options{IdleCompactAfter: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.Local)
	fill := func(person string, last time.Time) *Conversation {
		conversation := book.Conversation(person)
		for i := range 5 {
			conversation.appendTurn(ConversationTurn{At: last.Add(time.Duration(i-4) * time.Minute),
				Messages: []ChatMessage{{Role: RoleUser, Text: fmt.Sprintf("第 %d 句", i+1)}, {Role: RoleAssistant, Text: "好。"}}}, 1000, false)
		}
		return conversation
	}
	idle := fill("主人", now.Add(-2*time.Hour))
	recent := fill("老大", now.Add(-10*time.Minute))
	busy := fill("老二", now.Add(-3*time.Hour))
	busy.keepOpenTurn(ConversationTurn{At: now.Add(-3 * time.Hour), Messages: []ChatMessage{{Role: RoleUser, Text: "还在查"}}})
	unknown := book.Conversation("老三")
	for range 5 {
		unknown.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "旧记录"}}}, 1000, false)
	}

	book.compactIdle(now)
	turns := func(c *Conversation) int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.history) }
	if got := turns(idle); got != compactKeepTurns {
		t.Errorf("闲了 2 小时：应只留最近 %d 轮，留了 %d 轮", compactKeepTurns, got)
	}
	for name, c := range map[string]*Conversation{"闲了 10 分钟": recent, "正在进行一轮": busy, "不知道时间": unknown} {
		if got := turns(c); got != 5 {
			t.Errorf("%s：不该压，留了 %d 轮", name, got)
		}
	}
	if last := idle.lastTurnAt(); !last.Equal(now.Add(-2 * time.Hour)) {
		t.Errorf("压缩后最近一轮的时间不变：%v", last)
	}
	book.compactIdle(now.Add(time.Hour))
	if got := turns(idle); got != compactKeepTurns {
		t.Errorf("压过的不再重复压：留了 %d 轮", got)
	}
}

// 「过了多久」的说法：不到一分钟、分钟、小时（整点不写分钟）、天。
func TestElapsedText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Second:                "不到 1 分钟",
		45 * time.Minute:                "45 分钟",
		3 * time.Hour:                   "3 小时",
		3*time.Hour + 12*time.Minute:    "3 小时 12 分钟",
		2*24*time.Hour + 5*time.Hour:    "2 天 5 小时",
		3*24*time.Hour + 20*time.Minute: "3 天",
	} {
		if got := ElapsedText(d); got != want {
			t.Errorf("%v：%q，应是 %q", d, got, want)
		}
	}
}

// 彻底删账号：对话簿忘掉这个人以后，已经拿在手里的那份再写也不写盘；再要时重新从存储读（已经删了 = 空的）；别人的不动。
func TestConversationBookForget(t *testing.T) {
	store := &memoryStore{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	book := NewConversationBook(store, Options{Personas: bookPersonas, DefaultPersona: "buddy"}, logger)
	leaving, staying := book.Conversation("走的人"), book.Conversation("别人")
	turn := ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "你好"}, {Role: RoleAssistant, Text: "你好呀。"}}}
	leaving.appendTurn(turn, 0, false)
	staying.appendTurn(turn, 0, false)
	book.Forget("走的人")
	store.mu.Lock()
	delete(store.records, "走的人\x00") // 存储那边删掉（彻底删账号时，忘掉之后删）
	store.mu.Unlock()
	leaving.appendTurn(turn, 0, false) // 手里还拿着的：照样能用，但不写盘
	if _, ok, _ := store.LoadConversation("走的人", VoiceThread); ok {
		t.Error("忘掉以后不该再写盘")
	}
	if fresh := book.Conversation("走的人"); fresh == leaving || len(fresh.messages()) != 0 {
		t.Errorf("再要时应是新的、空的：%v", fresh.messages())
	}
	if book.Conversation("别人") != staying {
		t.Error("别人的不动")
	}
}
