package conversationstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/sqliteschema"
)

// numberedTurn 是第 seq 轮：主人说 question、助手回 answer。
func numberedTurn(seq int, question, answer string) brain.ConversationTurn {
	return brain.ConversationTurn{Seq: seq, At: time.Unix(int64(1_700_000_000+seq), 0),
		Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: question}, {Role: brain.RoleAssistant, Text: answer}}}
}

// transcriptOf 读一个人完整的对话记录：轮次编号 → 助手那句。
func transcriptOf(t *testing.T, store *SQLiteStore, person string) map[int]string {
	t.Helper()
	rows, err := store.db.Query(`SELECT seq, messages FROM transcript WHERE person = ? ORDER BY seq`, person)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	answers := map[int]string{}
	for rows.Next() {
		var seq int
		var data string
		if err := rows.Scan(&seq, &data); err != nil {
			t.Fatal(err)
		}
		var messages []brain.ChatMessage
		if err := json.Unmarshal([]byte(data), &messages); err != nil {
			t.Fatal(err)
		}
		answers[seq] = messages[len(messages)-1].Text
	}
	return answers
}

// 完整的对话记录只增不删：掉出窗口、压进摘要的轮次都还在；同一轮被打断后改成主人听到的部分时跟着更新；
// 没编号的不写进去。
func TestTranscriptKeepsEveryTurn(t *testing.T) {
	store, _ := openTestStore(t)
	record := brain.ConversationRecord{Person: "主人", TurnCount: 3, UpdatedAt: time.Now(),
		History: []brain.ConversationTurn{numberedTurn(1, "一", "答一"), numberedTurn(2, "二", "答二"), numberedTurn(3, "三", "答三")}}
	if err := store.SaveConversation(record); err != nil {
		t.Fatal(err)
	}
	record.TurnCount, record.Summary = 5, "聊过一、二"
	record.History = []brain.ConversationTurn{numberedTurn(3, "三", "答三"), numberedTurn(4, "四", "答四"),
		numberedTurn(5, "五", "答五（完整）"), {Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "没编号"}}}}
	if err := store.SaveConversation(record); err != nil {
		t.Fatal(err)
	}
	record.History[2] = numberedTurn(5, "五", "答五（听到的部分）")
	if err := store.SaveConversation(record); err != nil {
		t.Fatal(err)
	}
	got := transcriptOf(t, store, "主人")
	want := map[int]string{1: "答一", 2: "答二", 3: "答三", 4: "答四", 5: "答五（听到的部分）"}
	if len(got) != len(want) {
		t.Fatalf("完整记录应有 %d 轮：%v", len(want), got)
	}
	for seq, answer := range want {
		if got[seq] != answer {
			t.Errorf("第 %d 轮：%q，应为 %q", seq, got[seq], answer)
		}
	}
	loaded, ok, err := store.LoadConversation("主人")
	if err != nil || !ok || loaded.TurnCount != 5 || loaded.History[0].Seq != 3 || loaded.History[3].Seq != 0 {
		t.Errorf("轮次编号和编到第几都要存下：%+v %v", loaded, err)
	}
}

// 加完整记录之前的库：已有的轮次按先后编号（等压进摘要的在前），写进完整记录；编到第几接着往下。
func TestTranscriptMigrationNumbersExistingTurns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conversations.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := fs.ReadFile(migrationFiles, "migrations/00001_conversations.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := sqliteschema.Migrate(ctx, old, fstest.MapFS{"migrations/00001_conversations.sql": {Data: first}}); err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `INSERT INTO conversations (person, updated_at) VALUES ('主人', 1);
		INSERT INTO turns (person, part, position, at, messages) VALUES
			('主人', 'history', 0, 3, '[{"role":"assistant","text":"最近一"}]'),
			('主人', 'history', 1, 4, '[{"role":"assistant","text":"最近二"}]'),
			('主人', 'unsummarized', 0, 1, '[{"role":"assistant","text":"较早一"}]'),
			('主人', 'unsummarized', 1, 2, '[{"role":"assistant","text":"较早二"}]');`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	store, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("升级旧的对话库：%v", err)
	}
	defer store.Close()
	got := transcriptOf(t, store, "主人")
	for seq, answer := range map[int]string{1: "较早一", 2: "较早二", 3: "最近一", 4: "最近二"} {
		if got[seq] != answer {
			t.Errorf("第 %d 轮：%q，应为 %q（全部：%v）", seq, got[seq], answer, got)
		}
	}
	record, ok, err := store.LoadConversation("主人")
	if err != nil || !ok || record.TurnCount != 4 || record.Unsummarized[0].Seq != 1 || record.History[1].Seq != 4 {
		t.Errorf("迁移后读出来的轮次应带编号、编到第 4 轮：%+v %v", record, err)
	}
}
