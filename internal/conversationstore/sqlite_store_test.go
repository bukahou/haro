package conversationstore

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func openTestStore(t *testing.T) (*SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conversations.db")
	store, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func sampleRecord(person string) brain.ConversationRecord {
	at := time.Date(2026, 11, 18, 11, 59, 0, 0, time.UTC)
	return brain.ConversationRecord{Person: person, PersonaID: "gentle", Summary: "主人的车停在 B2。", ToolCallCount: 3,
		History: []brain.ConversationTurn{
			{At: at, Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "车停在哪"},
				{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "call_3", Name: "search_notes", Arguments: map[string]any{"query": "车"}}}},
				{Role: brain.RoleTool, ToolName: "search_notes", ToolCallID: "call_3", Text: "B2"},
				{Role: brain.RoleUser, Text: "（当时看了画面）", Synthetic: true},
				{Role: brain.RoleAssistant, Text: "在 B2。"}}},
			{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "旧的一轮"}, {Role: brain.RoleAssistant, Text: "好。"}}},
		},
		Unsummarized: []brain.ConversationTurn{{At: at.Add(-time.Hour), Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "更早"}}}},
		OpenTurn:     &brain.ConversationTurn{At: at.Add(time.Minute), Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "没说完的"}}},
		UpdatedAt:    at.Add(2 * time.Minute)}
}

// 存进去的读出来一模一样（时间、工具往来、系统合成的标记、没收尾的一轮）；整份替换，再存少的不留旧的；文件只有自己能读。
func TestSQLiteStoreRoundTrip(t *testing.T) {
	store, path := openTestStore(t)
	if _, ok, err := store.LoadConversation("主人"); ok || err != nil {
		t.Fatalf("没存过时应 ok=false：%v %v", ok, err)
	}
	want := sampleRecord("主人")
	if err := store.SaveConversation(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.LoadConversation("主人")
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	normalize := func(r *brain.ConversationRecord) {
		r.UpdatedAt = r.UpdatedAt.UTC()
		for _, turns := range [][]brain.ConversationTurn{r.History, r.Unsummarized} {
			for i := range turns {
				turns[i].At = turns[i].At.UTC()
			}
		}
		if r.OpenTurn != nil {
			r.OpenTurn.At = r.OpenTurn.At.UTC()
		}
	}
	normalize(&got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("读回来不一样\n got %+v\nwant %+v", got, want)
	}

	shorter := want
	shorter.History, shorter.Unsummarized, shorter.OpenTurn = shorter.History[:1], nil, nil
	if err := store.SaveConversation(shorter); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.LoadConversation("主人"); len(got.History) != 1 || got.Unsummarized != nil || got.OpenTurn != nil {
		t.Errorf("再存时应整份替换：%+v", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("对话库应只有自己能读：%v %v", info.Mode(), err)
	}
}

// 同时保存不同的人：各存各的、都在（单个连接里排队，不会互相覆盖）。
func TestSQLiteStoreConcurrentSaves(t *testing.T) {
	store, _ := openTestStore(t)
	var wg sync.WaitGroup
	for _, person := range []string{"主人", "家人", "客人", "_shared"} {
		for range 10 {
			wg.Go(func() {
				if err := store.SaveConversation(sampleRecord(person)); err != nil {
					t.Errorf("保存 %s: %v", person, err)
				}
			})
		}
	}
	wg.Wait()
	for _, person := range []string{"主人", "家人", "客人", "_shared"} {
		if got, ok, err := store.LoadConversation(person); !ok || err != nil || len(got.History) != 2 {
			t.Errorf("%s 的对话应完整：%v %v %+v", person, ok, err, got.History)
		}
	}
}

// 旧的 JSON 文件：导入后改名留底（不删）；库里已经有的人不覆盖；坏文件让导入报错（服务端起不来），文件原样留着。
func TestImportJSONDir(t *testing.T) {
	store, _ := openTestStore(t)
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("主人.json", `{"person":"主人","persona_id":"gentle","history":[[{"role":"user","text":"你好"},{"role":"assistant","text":"你好呀。"}]],"tool_call_count":2,"updated_at":"2026-11-17T10:00:00Z"}`)
	write("家人.json", `{"person":"家人","history":[[{"role":"user","text":"旧的"},{"role":"assistant","text":"旧的回答"}]]}`)
	write("主人.json.tmp", `半个文件`)
	newer := sampleRecord("家人")
	if err := store.SaveConversation(newer); err != nil {
		t.Fatal(err)
	}

	imported, err := ImportJSONDir(context.Background(), store, dir, discard)
	if err != nil || imported != 1 {
		t.Fatalf("应导入 1 个人：%d %v", imported, err)
	}
	owner, ok, _ := store.LoadConversation("主人")
	if !ok || owner.PersonaID != "gentle" || owner.ToolCallCount != 2 || len(owner.History) != 1 ||
		!owner.History[0].At.IsZero() || owner.History[0].Messages[1].Text != "你好呀。" {
		t.Errorf("导入的对话：%+v", owner)
	}
	if family, _, _ := store.LoadConversation("家人"); len(family.History) != 2 {
		t.Errorf("库里已经有的人不应被旧文件覆盖：%+v", family.History)
	}
	for _, name := range []string{"主人.json", "家人.json"} {
		if _, err := os.Stat(filepath.Join(dir, name+importedSuffix)); err != nil {
			t.Errorf("%s 应改名留底：%v", name, err)
		}
	}
	if again, err := ImportJSONDir(context.Background(), store, dir, discard); again != 0 || err != nil {
		t.Errorf("改了名的不再导入：%d %v", again, err)
	}

	write("坏的.json", `{不是 JSON`)
	if _, err := ImportJSONDir(context.Background(), store, dir, discard); err == nil {
		t.Error("坏文件应让导入报错")
	}
	if _, err := os.Stat(filepath.Join(dir, "坏的.json")); err != nil {
		t.Errorf("导入出错时旧文件应原样留着：%v", err)
	}
	if n, err := ImportJSONDir(context.Background(), store, filepath.Join(dir, "不存在"), discard); n != 0 || err != nil {
		t.Errorf("目录不存在 = 没有要导入的：%d %v", n, err)
	}
}
