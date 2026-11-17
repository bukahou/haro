package brain

import (
	"io"
	"log/slog"
	"sync"
	"testing"
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
	owner.appendTurn([]ChatMessage{{Role: RoleUser, Text: "你好"}, {Role: RoleAssistant, Text: "你好呀。"}}, 6)
	owner.setPersona(&bookPersonas[1])
	if id := owner.nextToolCallID(); id != "call_1" {
		t.Errorf("工具调用 ID：%s", id)
	}
	owner.appendTurn([]ChatMessage{{Role: RoleUser, Text: "再见"}, {Role: RoleAssistant, Text: "拜拜。"}}, 6)
	if book.Conversation("主人") != owner {
		t.Error("同一个人应拿到同一份对话")
	}
	family := book.Conversation("家人")
	if len(family.messages()) != 0 || family.currentPersona().ID != "buddy" {
		t.Errorf("别人的对话应是空的、用默认性格：%v %v", family.messages(), family.currentPersona())
	}

	restarted := NewConversationBook(store, opts, logger).Conversation("主人")
	if got := restarted.messages(); len(got) != 4 || got[3].Text != "拜拜。" {
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
		conversation.appendTurn([]ChatMessage{{Role: RoleUser, Text: text}, {Role: RoleAssistant, Text: text + "。"}}, 2)
	}
	if !conversation.replaceLastAnswer("听到一半") {
		t.Fatal("应能改最近一轮的回答")
	}
	got := conversation.messages()
	if len(got) != 4 || got[0].Text != "二" || got[3].Text != "听到一半" {
		t.Errorf("只留最近 2 轮，最后的回答被改掉：%+v", got)
	}
}
