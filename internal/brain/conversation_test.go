package brain

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// memoryStore 是测试用的对话存储。
type memoryStore struct {
	mu      sync.Mutex
	records map[string]ConversationRecord
}

func (m *memoryStore) LoadConversation(person string) (ConversationRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[person]
	return record, ok, nil
}

func (m *memoryStore) SaveConversation(record ConversationRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records == nil {
		m.records = map[string]ConversationRecord{}
	}
	m.records[record.Person] = record
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
	owner.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "你好"}, {Role: RoleAssistant, Text: "你好呀。"}}}, 6)
	owner.setPersona(&bookPersonas[1])
	if id := owner.nextToolCallID(); id != "call_1" {
		t.Errorf("工具调用 ID：%s", id)
	}
	owner.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "再见"}, {Role: RoleAssistant, Text: "拜拜。"}}}, 6)
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
	book := NewConversationBook(nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conversation := book.Conversation("")
	if conversation.Person() != SharedPerson {
		t.Errorf("没有名字时用全家的：%q", conversation.Person())
	}
	for _, text := range []string{"一", "二", "三"} {
		conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: text}, {Role: RoleAssistant, Text: text + "。"}}}, 2)
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
	conversation.appendTurn(ConversationTurn{At: at, Messages: []ChatMessage{{Role: RoleUser, Text: "明天天气怎么样"}, {Role: RoleAssistant, Text: "19 号晴。"}}}, 6)
	conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "旧的一轮"}, {Role: RoleAssistant, Text: "好。"}}}, 6)

	got := conversation.messages()
	if len(got) != 6 || !got[0].Synthetic || got[0].Role != RoleUser || got[0].Text != turnTimeLabel(at) || got[1].Text != "明天天气怎么样" {
		t.Fatalf("每轮前面应有那一轮的时间标记：%+v", got)
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
	conversation.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "你好"}, {Role: RoleAssistant, Text: "你好呀。"}}}, 6)
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
	if record, _, _ := store.LoadConversation("主人"); record.OpenTurn != nil || len(record.History) != 2 {
		t.Errorf("接上之后应写盘、清掉没收尾的记录：%+v", record)
	}
	// 正常收尾的一轮会替掉没收尾的记录
	restarted.keepOpenTurn(open)
	restarted.appendTurn(ConversationTurn{Messages: []ChatMessage{{Role: RoleUser, Text: "再见"}, {Role: RoleAssistant, Text: "拜拜。"}}}, 6)
	if record, _, _ := store.LoadConversation("主人"); record.OpenTurn != nil {
		t.Errorf("正常收尾后不应留着没收尾的记录：%+v", record.OpenTurn)
	}
}
