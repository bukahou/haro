package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/notes"
)

// scriptedEmbedder 是嵌入服务的替身：每段文字的向量由测试指定（不按内容自己算）；没指定的报错；down 时一律连不上。
type scriptedEmbedder struct {
	mu      sync.Mutex
	vectors map[string][]float32
	down    bool
	calls   int
}

func (e *scriptedEmbedder) vector(text string) ([]float32, error) {
	if vector, ok := e.vectors[text]; ok {
		return vector, nil
	}
	return nil, fmt.Errorf("测试没给「%s」的向量", text)
}

func (e *scriptedEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.down {
		return nil, errors.New("嵌入服务连不上")
	}
	return e.vector(text)
}

func (e *scriptedEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.down {
		return nil, errors.New("嵌入服务连不上")
	}
	var vectors [][]float32
	for _, text := range texts {
		vector, err := e.vector(text)
		if err != nil {
			return nil, err
		}
		vectors = append(vectors, vector)
	}
	return vectors, nil
}

func (e *scriptedEmbedder) Model() string { return "test-model" }

func (e *scriptedEmbedder) setDown(down bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.down = down
}

func (e *scriptedEmbedder) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func openNotes(t *testing.T) notes.Repository {
	t.Helper()
	store, err := notes.OpenSQLite(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// 方向：x 轴 = 吃的，y 轴 = 车，z 轴 = 学校。
var embeddings = map[string][]float32{
	"不吃香菜":          {1, 0, 0},
	"车停在 C3 的 7 号位": {0, 1, 0},
	"钥匙在玄关的抽屉里":     {0.2, 0.2, 0},
	"周五要交作业":        {0, 0, 1},
	"我有什么忌口":        {0.9, 0.1, 0},
	"我车停哪了":         {0, 1, 0.1},
}

// 记住时当场算向量；按意思找：最像的在前，只找自己的和全家共用的；忘掉的找不到，恢复后又找得到（向量还在库里，不用重算）。
func TestRememberRecallForgetRestore(t *testing.T) {
	ctx := context.Background()
	store := openNotes(t)
	embedder := &scriptedEmbedder{vectors: embeddings}
	keeper := NewKeeper(store, embedder, nil, 20, quiet)
	food, err := keeper.Remember(ctx, "主人", "不吃香菜", notes.SourceModel, true)
	if err != nil {
		t.Fatal(err)
	}
	car, _ := keeper.Remember(ctx, "主人", "车停在 C3 的 7 号位", notes.SourceOwner, false)
	if _, err := store.Save(ctx, notes.NewNote{Content: "钥匙在玄关的抽屉里", Source: notes.SourceOwner}); err != nil { // 全家共用（旧记事）
		t.Fatal(err)
	}
	_, _ = keeper.Remember(ctx, "老大", "周五要交作业", notes.SourceOwner, false)
	keeper.indexPending(ctx) // 全家共用那条由后台补

	recollection, err := keeper.Recall(ctx, "主人", "我有什么忌口")
	if err != nil || !recollection.ByMeaning || len(recollection.Notes) != 3 || recollection.Notes[0].ID != food.ID {
		t.Fatalf("按意思找：最像的（忌口）在前，别人的不出现：%+v %v", recollection, err)
	}
	for _, note := range recollection.Notes {
		if note.Person == "老大" {
			t.Errorf("别人的记事不该出现：%+v", note)
		}
	}
	if _, err := keeper.Forget(ctx, "主人", car.ID); err != nil {
		t.Fatal(err)
	}
	if recollection, _ := keeper.Recall(ctx, "主人", "我车停哪了"); len(recollection.Notes) > 0 && recollection.Notes[0].ID == car.ID {
		t.Errorf("忘掉的找不到：%+v", recollection.Notes)
	}
	calls := embedder.callCount()
	if _, err := keeper.Restore(ctx, "主人", car.ID); err != nil {
		t.Fatal(err)
	}
	if embedder.callCount() != calls {
		t.Error("恢复时用库里的向量，不用重算")
	}
	if recollection, _ := keeper.Recall(ctx, "主人", "我车停哪了"); len(recollection.Notes) == 0 || recollection.Notes[0].ID != car.ID {
		t.Errorf("恢复后又找得到：%+v", recollection.Notes)
	}
	if _, err := keeper.Remember(ctx, "主人", strings.Repeat("长", maxContentRunes+1), notes.SourceOwner, false); !errors.Is(err, ErrTooLong) {
		t.Errorf("太长的不记：%v", err)
	}
}

// 嵌入服务连不上：照样记下，「查」只给最近记的并写明原因；恢复以后后台补上向量，又能按意思找。
func TestEmbedderDownStillRemembers(t *testing.T) {
	ctx := context.Background()
	embedder := &scriptedEmbedder{vectors: embeddings, down: true}
	keeper := NewKeeper(openNotes(t), embedder, nil, 20, quiet)
	note, err := keeper.Remember(ctx, "主人", "不吃香菜", notes.SourceModel, false)
	if err != nil {
		t.Fatalf("嵌入服务连不上也要记下：%v", err)
	}
	recollection, err := keeper.Recall(ctx, "主人", "我有什么忌口")
	if err != nil || recollection.ByMeaning || recollection.Note == "" || len(recollection.Notes) != 1 {
		t.Fatalf("连不上时只给最近记的、写明原因：%+v %v", recollection, err)
	}
	embedder.setDown(false)
	keeper.indexPending(ctx)
	if recollection, _ := keeper.Recall(ctx, "主人", "我有什么忌口"); !recollection.ByMeaning || len(recollection.Notes) != 1 ||
		recollection.Notes[0].ID != note.ID {
		t.Errorf("后台补上向量后能按意思找：%+v", recollection)
	}
}

// 没配嵌入服务：照样能记、列、忘、恢复；「查」只给最近记的；后台什么都不做。
func TestWithoutEmbedder(t *testing.T) {
	ctx := context.Background()
	keeper := NewKeeper(openNotes(t), nil, nil, 20, quiet)
	if keeper.SearchesByMeaning() {
		t.Fatal("没配嵌入服务时不能按意思找")
	}
	if _, err := keeper.Remember(ctx, "主人", "不吃香菜", notes.SourceModel, false); err != nil {
		t.Fatal(err)
	}
	if recollection, _ := keeper.Recall(ctx, "主人", "忌口"); recollection.ByMeaning || len(recollection.Notes) != 1 {
		t.Errorf("只给最近记的：%+v", recollection)
	}
	done := make(chan struct{})
	go func() { keeper.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("没配嵌入服务时 Run 应马上返回")
	}
}

// 以前的对话：后台补向量（已有的记录也补），按意思找到这个人自己的；文字变了的那一轮重算；重启后从库里载入、不用重算。
func TestRecallFindsPastConversations(t *testing.T) {
	ctx := context.Background()
	conversations, err := conversationstore.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer conversations.Close()
	turn := func(seq int, said, answered string) brain.ConversationTurn {
		return brain.ConversationTurn{Seq: seq, At: time.Unix(int64(1_700_000_000+seq), 0),
			Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: said}, {Role: brain.RoleAssistant, Text: answered}}}
	}
	for _, record := range []brain.ConversationRecord{
		{Person: "主人", TurnCount: 2, UpdatedAt: time.Now(), History: []brain.ConversationTurn{
			turn(1, "关西学院哪个学部有名", "法学部和经济学部"), turn(2, "今天几号", "26 号")}},
		{Person: "老大", TurnCount: 1, UpdatedAt: time.Now(), History: []brain.ConversationTurn{turn(1, "关西学院怎么样", "很好")}},
	} {
		if err := conversations.SaveConversation(record); err != nil {
			t.Fatal(err)
		}
	}
	vectors := map[string][]float32{
		"主人：关西学院哪个学部有名\nharo：法学部和经济学部": {0, 0, 1},
		"主人：今天几号\nharo：26 号":           {1, 0, 0},
		"主人：关西学院怎么样\nharo：很好":          {0, 0, 1},
		"上次聊的那个大学":                     {0, 0.1, 1},
	}
	embedder := &scriptedEmbedder{vectors: vectors}
	keeper := NewKeeper(openNotes(t), embedder, conversations, 20, quiet)
	keeper.load(ctx)
	keeper.indexPending(ctx)
	recollection, err := keeper.Recall(ctx, "主人", "上次聊的那个大学")
	if err != nil || len(recollection.Turns) != 2 || recollection.Turns[0].Answered != "法学部和经济学部" {
		t.Fatalf("应找到主人自己的那一轮（最像的在前），别人的不出现：%+v %v", recollection.Turns, err)
	}
	for _, found := range recollection.Turns {
		if found.Person != "主人" {
			t.Errorf("别人的对话不该出现：%+v", found)
		}
	}

	calls := embedder.callCount()
	restarted := NewKeeper(openNotes(t), embedder, conversations, 20, quiet)
	restarted.load(ctx)
	restarted.indexPending(ctx)
	if embedder.callCount() != calls {
		t.Error("重启后从库里载入，不用重算")
	}
	if recollection, _ := restarted.Recall(ctx, "主人", "上次聊的那个大学"); len(recollection.Turns) == 0 {
		t.Error("重启后照样找得到")
	}
}

// 常驻记忆每轮附在情况说明里：新的在前，最多 residentMaxItems 条、一共 residentMaxRunes 字；没有常驻的不附。
func TestResidentBriefing(t *testing.T) {
	ctx := context.Background()
	keeper := NewKeeper(openNotes(t), nil, nil, 20, quiet)
	if briefing := keeper.Brief("主人"); len(briefing.Lines) != 0 {
		t.Fatalf("没有常驻的不附：%+v", briefing)
	}
	_, _ = keeper.Remember(ctx, "主人", "车停在 C3", notes.SourceOwner, false) // 不常驻
	for i := range residentMaxItems + 3 {
		if _, err := keeper.Remember(ctx, "主人", fmt.Sprintf("偏好%02d", i), notes.SourceModel, true); err != nil {
			t.Fatal(err)
		}
	}
	briefing := keeper.Brief("主人")
	if len(briefing.Lines) != 1 || strings.Contains(briefing.Lines[0], "C3") || strings.Count(briefing.Lines[0], "偏好") != residentMaxItems ||
		!strings.Contains(briefing.Lines[0], fmt.Sprintf("偏好%02d", residentMaxItems+2)) {
		t.Fatalf("新的在前、最多 %d 条、不常驻的不附：%+v", residentMaxItems, briefing)
	}
	long := NewKeeper(openNotes(t), nil, nil, 20, quiet)
	for range 3 {
		_, _ = long.Remember(ctx, "主人", strings.Repeat("长", 190), notes.SourceModel, true)
	}
	if line := long.Brief("主人").Lines[0]; strings.Count(line, strings.Repeat("长", 190)) != 2 {
		t.Errorf("一共不超过 %d 字：超出的不附", residentMaxRunes)
	}
}

// 后台：记住新的一条时马上补（Poke），ctx 结束就退出。
func TestRunIndexesWhenPoked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := openNotes(t)
	embedder := &scriptedEmbedder{vectors: embeddings}
	keeper := NewKeeper(store, embedder, nil, 20, quiet)
	done := make(chan struct{})
	go func() { keeper.Run(ctx); close(done) }()
	if _, err := store.Save(ctx, notes.NewNote{Content: "钥匙在玄关的抽屉里", Source: notes.SourceOwner}); err != nil {
		t.Fatal(err)
	}
	keeper.Poke()
	deadline := time.Now().Add(2 * time.Second)
	for keeper.index.Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Poke 之后应马上补上向量")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 结束后应退出")
	}
}

// slowEmbedder 算问句的向量时一直不回（嵌入服务卡住），直到 ctx 结束。
type slowEmbedder struct{ scriptedEmbedder }

func (e *slowEmbedder) EmbedQuery(ctx context.Context, _ string) ([]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// 查的时候嵌入服务卡住：最多等 queryEmbedTimeout，就只给最近记的（主人在等回答，不能一直等）。
func TestRecallDoesNotWaitForStuckEmbedder(t *testing.T) {
	ctx := context.Background()
	embedder := &slowEmbedder{scriptedEmbedder{vectors: embeddings}}
	keeper := NewKeeper(openNotes(t), embedder, nil, 20, quiet)
	if _, err := keeper.Remember(ctx, "主人", "不吃香菜", notes.SourceModel, false); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	recollection, err := keeper.Recall(ctx, "主人", "我有什么忌口")
	if elapsed := time.Since(started); err != nil || recollection.ByMeaning || len(recollection.Notes) != 1 ||
		elapsed > queryEmbedTimeout+time.Second {
		t.Fatalf("卡住时等 %v 左右就只给最近记的：%+v %v（等了 %v）", queryEmbedTimeout, recollection, err, elapsed)
	}
}
