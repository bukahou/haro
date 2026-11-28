package backstory

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeMerger 记下收到的，交回「现在的 + 每轮一条」；整理时（没有新轮次）只留最后一条。
type fakeMerger struct {
	mu    sync.Mutex
	calls int
	kept  []Entry
}

func (f *fakeMerger) Merge(_ context.Context, kept, current []Entry, turns []conversationstore.FoundTurn) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.kept = kept
	if len(turns) == 0 {
		return current[len(current)-1:], nil
	}
	out := append([]Entry{}, current...)
	for _, turn := range turns {
		out = append(out, Entry{Text: "聊过：" + turn.Messages[0].Text, SourceSeq: turn.Seq})
	}
	return out, nil
}

// turnAt 是第 seq 轮说话的时间（固定：同一轮再存一次时间不变）。
func turnAt(seq int) time.Time { return time.Unix(1_800_000_000+int64(seq)*60, 0) }

func saveTurns(t *testing.T, store *conversationstore.SQLiteStore, person, thread string, seqs ...int) {
	t.Helper()
	record := brain.ConversationRecord{Person: person, Thread: thread, TurnCount: seqs[len(seqs)-1], UpdatedAt: turnAt(seqs[len(seqs)-1])}
	for _, seq := range seqs {
		record.History = append(record.History, brain.ConversationTurn{Seq: seq, At: turnAt(seq),
			Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "第" + string(rune('0'+seq)) + "句"}, {Role: brain.RoleAssistant, Text: "好"}}})
	}
	if err := store.SaveConversation(record); err != nil {
		t.Fatal(err)
	}
}

// 闲下来才并（刚说完的不并）；并进去的带出处；主人改过的不交给代理改、后台不动；整理一天一次；关了就不带也不并。
func TestMergeEditConsolidateDisable(t *testing.T) {
	ctx := context.Background()
	store, err := conversationstore.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := turnAt(1).Add(10 * time.Minute)
	merger := &fakeMerger{}
	service, err := Open(ctx, store, merger, time.Hour, quiet)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	saveTurns(t, store, "主人", brain.VoiceThread, 1)
	service.Tick(ctx)
	if merger.calls != 0 || service.Block("主人") != "" {
		t.Fatal("刚说完、还没闲下来：不并")
	}
	now = now.Add(2 * time.Hour)
	service.Tick(ctx)
	if merger.calls != 2 {
		t.Errorf("闲下来：并一次，还没整理过的再整理一次（%d 次）", merger.calls)
	}
	view, used, err := service.View(ctx, "主人")
	if err != nil || len(view.Items) != 1 || view.Items[0].SourceSeq != 1 || view.MergedSeq != 1 || used == 0 || view.ConsolidatedAt == nil {
		t.Fatalf("闲下来并进去：%+v %d %v", view, used, err)
	}
	if block := service.Block("主人"); !strings.Contains(block, "聊过：第1句") {
		t.Errorf("提示词里带着：%q", block)
	}
	if err := service.Edit(ctx, "主人", view.Items[0].ID, "主人改过的"); err != nil {
		t.Fatal(err)
	}
	saveTurns(t, store, "主人", brain.VoiceThread, 1, 2)
	now = turnAt(2).Add(2 * time.Hour)
	merger.calls = 0
	service.Tick(ctx)
	view, _, _ = service.View(ctx, "主人")
	if len(merger.kept) != 1 || merger.kept[0].Text != "主人改过的" || len(view.Items) != 2 || view.Items[0].Text != "主人改过的" || !view.Items[0].Edited {
		t.Fatalf("主人改过的原样留着、只读交给代理：%+v kept=%+v", view.Items, merger.kept)
	}
	if merger.calls != 1 {
		t.Errorf("一天之内整理过了：这一轮只并（%d 次）", merger.calls)
	}
	calls := merger.calls
	service.Tick(ctx)
	if merger.calls != calls {
		t.Error("一天之内不再整理、没有新轮次不并")
	}
	if err := service.SetEnabled(ctx, "主人", false); err != nil {
		t.Fatal(err)
	}
	if service.Block("主人") != "" {
		t.Error("关了就不带")
	}
	saveTurns(t, store, "主人", brain.VoiceThread, 1, 2, 3)
	now = now.Add(48 * time.Hour)
	service.Tick(ctx)
	if merger.calls != calls {
		t.Error("关了就不并、不整理")
	}
}

// 无痕对话的轮次不并；提示词里放不下时从最旧的起不放。
func TestIncognitoAndLimit(t *testing.T) {
	ctx := context.Background()
	store, err := conversationstore.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateThread(ctx, "主人", "secret", true, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	saveTurns(t, store, "主人", "secret", 1)
	merger := &fakeMerger{}
	service, err := Open(ctx, store, merger, time.Minute, quiet)
	if err != nil {
		t.Fatal(err)
	}
	service.Tick(ctx)
	if merger.calls != 0 {
		t.Error("无痕的不并")
	}
	long := strings.Repeat("长", MaxRunes/2)
	items := []conversationstore.BackstoryItem{{ID: "old", Text: "最旧的" + long}, {ID: "mid", Text: "中间的" + long}, {ID: "new", Text: "最新的"}}
	if block := render(items); strings.Contains(block, "最旧的") || !strings.Contains(block, "最新的") || !strings.Contains(block, "中间的") {
		t.Errorf("放不下时从最旧的起不放：%d 字", len([]rune(block)))
	}
}

// 第一次建背景记录池：只从最近 firstBackfill 轮开始并（更早的不并）。
func TestFirstBackfillIsBounded(t *testing.T) {
	ctx := context.Background()
	store, err := conversationstore.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seqs := make([]int, 0, firstBackfill+20)
	for seq := 1; seq <= firstBackfill+20; seq++ {
		seqs = append(seqs, seq)
	}
	saveTurns(t, store, "主人", brain.VoiceThread, seqs...)
	merger := &fakeMerger{}
	service, err := Open(ctx, store, merger, time.Minute, quiet)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return turnAt(len(seqs)).Add(time.Hour) }
	service.Tick(ctx)
	view, _, _ := service.View(ctx, "主人")
	if want := int64(20 + mergeBatch); view.MergedSeq != want { // 从第 21 轮起并一批
		t.Errorf("第一次只从最近 %d 轮起并：并到了第 %d 轮，应是第 %d 轮", firstBackfill, view.MergedSeq, want)
	}
}
