package conversationstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

func TestFileStoreRoundTrip(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "conversations"))
	if _, ok, err := store.LoadConversation("主人"); ok || err != nil {
		t.Fatalf("没存过应返回 ok=false：%v %v", ok, err)
	}
	record := brain.ConversationRecord{Person: "主人", PersonaID: "gentle", ToolCallCount: 3, UpdatedAt: time.Now(),
		History: [][]brain.ChatMessage{{
			{Role: brain.RoleUser, Text: "帮我记一下"},
			{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "call_3", Name: "save_note", Arguments: map[string]any{"text": "买牛奶"}}}},
			{Role: brain.RoleTool, ToolName: "save_note", ToolCallID: "call_3", Text: "ok"},
			{Role: brain.RoleAssistant, Text: "记下了。"},
		}}}
	if err := store.SaveConversation(record); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(store.dir, "主人.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("对话文件权限应为 0600：%v", info.Mode().Perm())
	}
	loaded, ok, err := store.LoadConversation("主人")
	if err != nil || !ok || loaded.PersonaID != "gentle" || loaded.ToolCallCount != 3 || len(loaded.History[0]) != 4 ||
		loaded.History[0][1].ToolCalls[0].Arguments["text"] != "买牛奶" || loaded.History[0][2].ToolCallID != "call_3" {
		t.Errorf("读回来应和存的一样：%+v %v %v", loaded, ok, err)
	}
	if err := store.SaveConversation(brain.ConversationRecord{Person: "../etc"}); err == nil {
		t.Error("不合法的名字应拒绝")
	}
	if _, _, err := store.LoadConversation(brain.SharedPerson); err != nil {
		t.Errorf("全家共用的对话名应合法：%v", err)
	}
}
