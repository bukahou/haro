package brain_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/conversationstore"
)

// storedTurn 是存在对话库里的第 seq 轮（一小时前开始、每轮隔一分钟）。
func storedTurn(seq int) brain.ConversationTurn {
	return brain.ConversationTurn{Seq: seq, At: time.Now().Add(-time.Hour + time.Duration(seq)*time.Minute).Truncate(time.Second),
		Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "旧问" + strconv.Itoa(seq)},
			{Role: brain.RoleAssistant, Text: "旧答" + strconv.Itoa(seq)}}}
}

// transcriptAnswers 读完整的对话记录：轮次编号 → 助手那句。
func transcriptAnswers(t *testing.T, db *sql.DB, person string) map[int]string {
	t.Helper()
	rows, err := db.Query(`SELECT seq, messages FROM transcript WHERE person = ?`, person)
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

// 读一个人的对话出错（这里是存坏了的一轮，解析不了）：这一轮不带之前的对话照常回答（附注里告诉模型记不住），
// 但不写盘——存着的摘要、最近几轮、完整的对话记录都不被盖掉；下一轮重读，读得出来就接上，新的一轮接着编号。
// 用真的对话库（SQLite）测。
func TestConversationLoadFailureDoesNotOverwriteStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conversations.db")
	store, err := conversationstore.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var history []brain.ConversationTurn
	for seq := 1; seq <= 5; seq++ { // 一轮一轮存（窗口 2 轮）：完整记录里攒下 1..5
		history = append(history, storedTurn(seq))
		if len(history) > 2 {
			history = history[len(history)-2:]
		}
		if err := store.SaveConversation(brain.ConversationRecord{Person: "主人", Summary: "主人的车停在 B2。", TurnCount: seq,
			History: history, UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", path) // 另开一个连接：弄坏一轮、之后再修好
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var good string
	if err := db.QueryRow(`SELECT messages FROM turns WHERE person = '主人' AND part = 'history' AND position = 0`).Scan(&good); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE turns SET messages = '{' WHERE person = '主人' AND part = 'history' AND position = 0`); err != nil {
		t.Fatal(err)
	}

	model := braintest.NewModel(t, braintest.Say("你好呀。"), braintest.Say("在 B2。"))
	options := brain.Options{MaxHistoryTurn: 2}
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{},
		Conversations: brain.NewConversationBook(store, options, quiet)}, options)

	k.owner("你好")
	first := model.Requests()[0]
	if got := userTexts(first); !slices.Equal(got, []string{"你好"}) || strings.Contains(first.System(), "B2") {
		t.Errorf("读不出之前的对话：这一轮不带历史和摘要：%v %q", got, first.System())
	}
	if !slices.ContainsFunc(first.Messages, func(m brain.ChatMessage) bool {
		return m.Synthetic && strings.Contains(m.Text, brain.ConversationUnreadableNote)
	}) {
		t.Error("这一轮的附注里要告诉模型之前的对话没读出来")
	}
	var summary string
	var turnCount int
	if err := db.QueryRow(`SELECT summary, turn_count FROM conversations WHERE person = '主人'`).Scan(&summary, &turnCount); err != nil {
		t.Fatal(err)
	}
	if summary != "主人的车停在 B2。" || turnCount != 5 {
		t.Errorf("读失败的那一轮不该写盘：摘要 %q、编到第 %d 轮", summary, turnCount)
	}
	want := map[int]string{1: "旧答1", 2: "旧答2", 3: "旧答3", 4: "旧答4", 5: "旧答5"}
	if got := transcriptAnswers(t, db, "主人"); !maps.Equal(got, want) {
		t.Errorf("完整的对话记录不该被改：%v", got)
	}

	if _, err := db.Exec(`UPDATE turns SET messages = ? WHERE person = '主人' AND part = 'history' AND position = 0`, good); err != nil {
		t.Fatal(err)
	}
	k.owner("车停在哪")
	second := model.Requests()[1]
	if got := userTexts(second); !slices.Equal(got, []string{"旧问4", "旧问5", "车停在哪"}) || !strings.Contains(second.System(), "B2") {
		t.Errorf("读得出来了：接上存着的对话（读失败那一轮不在里面）：%v %q", got, second.System())
	}
	record, ok, err := store.LoadConversation("主人", "")
	if err != nil || !ok || record.TurnCount != 6 || record.History[len(record.History)-1].Seq != 6 || record.Summary != "主人的车停在 B2。" {
		t.Fatalf("新的一轮接着编号（第 6 轮），摘要还在：%+v %v", record, err)
	}
	want[6] = "在 B2。"
	if got := transcriptAnswers(t, db, "主人"); !maps.Equal(got, want) {
		t.Errorf("完整的对话记录接着往下记：%v", got)
	}
}
