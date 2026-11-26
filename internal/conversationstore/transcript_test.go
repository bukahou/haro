package conversationstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
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

// 同一编号来了另一轮（说话的时间不同，例如对话从头编号了）：完整记录里原来那一轮不被盖掉；这份对话照样存下，
// 交回说明哪几轮没写进记录的错误。同一轮（时间相同）照常更新，见 TestTranscriptKeepsEveryTurn。
func TestTranscriptKeepsTurnWhenSeqIsReused(t *testing.T) {
	store, _ := openTestStore(t)
	var history []brain.ConversationTurn
	for seq := 1; seq <= 3; seq++ { // 一轮一轮存：完整记录里攒下 1..3
		history = append(history, numberedTurn(seq, "旧问", "旧答"+strconv.Itoa(seq)))
		if err := store.SaveConversation(brain.ConversationRecord{Person: "主人", Summary: "旧摘要", TurnCount: seq,
			History: history, UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	reused := brain.ConversationTurn{Seq: 1, At: time.Unix(1_800_000_000, 0),
		Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "新问"}, {Role: brain.RoleAssistant, Text: "新答"}}}
	err := store.SaveConversation(brain.ConversationRecord{Person: "主人", TurnCount: 1,
		History: []brain.ConversationTurn{reused}, UpdatedAt: time.Now()})
	var kept *TranscriptKeptError
	if !errors.As(err, &kept) || !slices.Equal(kept.Seqs, []int64{1}) {
		t.Errorf("编号 1 已经是另一轮：应交回没覆盖的是哪几轮：%v", err)
	}
	got := transcriptOf(t, store, "主人")
	for seq, answer := range map[int]string{1: "旧答1", 2: "旧答2", 3: "旧答3"} {
		if got[seq] != answer {
			t.Errorf("完整记录第 %d 轮被改了：%q，应为 %q", seq, got[seq], answer)
		}
	}
	if loaded, ok, err := store.LoadConversation("主人"); err != nil || !ok || loaded.TurnCount != 1 || len(loaded.History) != 1 {
		t.Errorf("这份对话照样存下：%+v %v", loaded, err)
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

// 完整记录的向量：没有向量的轮次要算；算过的不再要；这一轮的文字后来变了（被打断）就重算；换了模型都重算；
// 按编号取回主人说的和 haro 答的（系统附注不算主人说的）。外键：不存在的轮次存不进向量。
func TestTranscriptVectors(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	note := brain.ChatMessage{Role: brain.RoleUser, Text: "【现在的情况】到点提醒 #1", Synthetic: true}
	reminder := brain.ConversationTurn{Seq: 3, At: time.Unix(1_700_000_003, 0),
		Messages: []brain.ChatMessage{note, {Role: brain.RoleAssistant, Text: "该喝水了。"}}}
	record := brain.ConversationRecord{Person: "主人", TurnCount: 3, UpdatedAt: time.Now(),
		History: []brain.ConversationTurn{numberedTurn(1, "关西学院哪个学部有名", "法学部和经济学部"), numberedTurn(2, "二", "答二"), reminder}}
	if err := store.SaveConversation(record); err != nil {
		t.Fatal(err)
	}
	pending, err := store.UnindexedTurns(ctx, "e5", 10)
	if err != nil || len(pending) != 3 {
		t.Fatalf("三轮都要算：%+v %v", pending, err)
	}
	if pending[0].Said != "关西学院哪个学部有名" || pending[0].Answered != "法学部和经济学部" || pending[2].Said != "" ||
		pending[2].Text() != "haro：该喝水了。" {
		t.Errorf("主人说的、haro 答的（系统附注不算）：%+v", pending)
	}
	for _, turn := range pending {
		if err := store.SaveTurnVector(ctx, turn.Person, turn.Seq, "e5", turn.Digest(), []float32{float32(turn.Seq)}); err != nil {
			t.Fatal(err)
		}
	}
	if pending, _ := store.UnindexedTurns(ctx, "e5", 10); len(pending) != 0 {
		t.Fatalf("算过的不再要：%+v", pending)
	}
	record.History[1] = numberedTurn(2, "二", "答二（听到的部分）") // 第 2 轮被打断，改成听到的部分
	if err := store.SaveConversation(record); err != nil {
		t.Fatal(err)
	}
	if pending, _ := store.UnindexedTurns(ctx, "e5", 10); len(pending) != 1 || pending[0].Seq != 2 {
		t.Fatalf("文字变了的那一轮要重算：%+v", pending)
	}
	if pending, _ := store.UnindexedTurns(ctx, "bge", 10); len(pending) != 3 {
		t.Errorf("换了模型都要重算：%+v", pending)
	}
	vectors, err := store.TurnVectors(ctx, "e5")
	if err != nil || len(vectors) != 3 || vectors[0].Vector[0] != 1 {
		t.Errorf("向量：%+v %v", vectors, err)
	}
	turns, err := store.TranscriptTurns(ctx, "主人", []int64{2, 9, 1})
	if err != nil || len(turns) != 2 || turns[0].Seq != 2 || turns[1].Answered != "法学部和经济学部" || turns[1].At.IsZero() {
		t.Errorf("按编号取：%+v %v", turns, err)
	}
	if err := store.SaveTurnVector(ctx, "主人", 99, "e5", "x", []float32{1}); err == nil {
		t.Error("不存在的轮次不该存进向量（外键）")
	}
}
