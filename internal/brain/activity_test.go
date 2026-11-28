package brain_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// activityRecorder 记下打字会话报告的事（工具开始、做完、卡片）。
type activityRecorder struct {
	mu     sync.Mutex
	events []string
	cards  []brain.Card
}

func (a *activityRecorder) ToolStarted(callID, label string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "start "+callID+" "+label)
}

func (a *activityRecorder) ToolFinished(callID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "finish "+callID)
}

func (a *activityRecorder) CardShown(card brain.Card) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cards = append(a.cards, card)
}

func (a *activityRecorder) snapshot() ([]string, []brain.Card) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.events), slices.Clone(a.cards)
}

// textInput 在打字会话里交一句（可以是点卡片的决定），收事件直到这一轮结束。
func textInput(t *testing.T, session *brain.Session, input brain.TextInput) []realtimeproto.ServerEvent {
	t.Helper()
	if err := session.SubmitText(input); err != nil {
		t.Fatal(err)
	}
	var events []realtimeproto.ServerEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-session.Events():
			events = append(events, event)
			if _, done := event.(realtimeproto.ResponseDone); done {
				return events
			}
		case <-timeout:
			t.Fatalf("这一轮没结束：%v", events)
		}
	}
}

// 打字会话：每个工具开始、做完都告诉 App（带给人看的说明）；语音会话不设 Activity，不受影响。
func TestTextSessionReportsToolActivity(t *testing.T) {
	model := braintest.NewModel(t, searchCarSpot, braintest.Say("车停在C3的7号位。"))
	activity := &activityRecorder{}
	session := brain.NewSession(brain.Components{Front: withTestSteps(brain.Agent{Model: model}), Tools: &fakeToolbox{}, Thread: "t1",
		TextOnly: true, Activity: activity}, brain.Options{MaxHistoryTurn: 6}, quiet)
	t.Cleanup(func() { _ = session.Close() })
	textInput(t, session, brain.TextInput{Text: "车停在哪", Person: "主人", Language: "zh"})
	events, cards := activity.snapshot()
	if len(events) != 2 || !strings.HasPrefix(events[0], "start ") || !strings.HasSuffix(events[0], " 用工具") ||
		events[1] != "finish "+strings.Fields(events[0])[1] || len(cards) != 0 {
		t.Errorf("工具开始（没配说明的用通用的一句）、做完：%v %v", events, cards)
	}
}

// 打字会话里的确认：会破坏的调用存下、出一张确认卡片；模型不能代为确认（不给它 confirm_action，调用了也拒绝），
// 主人在卡片上点了确认才按存下的参数执行；卡片记在历史里那次结果上。
func TestTextSessionConfirmsOnlyFromCard(t *testing.T) {
	store, err := conversationstore.OpenSQLite(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	book := brain.NewConversationBook(store, brain.Options{MaxHistoryTurn: 6}, quiet)
	model := braintest.NewModel(t, deleteNoteThree, braintest.Say("要删掉第 3 条吗？点卡片确认。"),
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}), braintest.Say("要在卡片上点确认才行。"),
		braintest.Say("删好了。"))
	toolbox := &fakeToolbox{}
	turn := func(input brain.TextInput) *activityRecorder { // 每一句开一个打字会话（和 App 一样），对话簿共用
		activity := &activityRecorder{}
		session := brain.NewSession(brain.Components{Front: withTestSteps(brain.Agent{Model: model}), Tools: toolbox, Thread: "t1",
			TextOnly: true, Activity: activity, Conversations: book}, brain.Options{MaxHistoryTurn: 6}, quiet)
		t.Cleanup(func() { _ = session.Close() })
		textInput(t, session, input)
		return activity
	}

	asked := turn(brain.TextInput{Text: "把第 3 条删了", Person: "主人", Language: "zh"})
	_, cards := asked.snapshot()
	if len(cards) != 1 || cards[0].Kind != brain.CardConfirmation || cards[0].ActionID != "act_1" || cards[0].Tool != "delete_note" ||
		cards[0].ExpiresAt.IsZero() {
		t.Fatalf("应出一张确认卡片：%+v", cards)
	}
	if names := model.Requests()[0].ToolNames(); slices.Contains(names, "confirm_action") || !slices.Contains(names, "cancel_action") {
		t.Errorf("打字会话不给模型确认、照样给取消：%v", names)
	}
	if note := toolResultOf(t, model.Requests()[1])["note"].(string); !strings.Contains(note, "卡片") {
		t.Errorf("要告诉模型请主人点卡片：%q", note)
	}
	if !book.ActionOpen("主人", "t1", "act_1") {
		t.Error("问完之后那次操作应还在等确认")
	}

	turn(brain.TextInput{Text: "嗯，删吧", Person: "主人", Language: "zh"})
	if len(toolbox.called()) != 0 {
		t.Fatal("打字说同意不算：模型调用 confirm_action 应被拒绝")
	}
	if refused := toolResultOf(t, model.Requests()[3]); refused["status"] != "refused" {
		t.Errorf("模型代为确认应被拒绝：%v", refused)
	}

	turn(brain.TextInput{Person: "主人", Language: "zh", Decision: &brain.Decision{ActionID: "act_1", Confirm: true}})
	if got := toolbox.called(); !slices.Equal(got, []string{"delete_note"}) {
		t.Fatalf("点了卡片上的确认应执行：%v", got)
	}
	if done := toolResultOf(t, model.Requests()[4]); done["status"] != "done" {
		t.Errorf("执行结果应交给模型去说：%v", done)
	}
	if book.ActionOpen("主人", "t1", "act_1") {
		t.Error("确认过的卡片不该还能点")
	}
	record, ok, _ := store.LoadConversation("主人", "t1")
	if !ok || len(record.History) != 3 {
		t.Fatalf("三轮都应记进历史：%+v", record)
	}
	var carded *brain.Card
	for _, message := range record.History[0].Messages {
		if message.Card != nil {
			carded = message.Card
		}
	}
	if carded == nil || carded.ActionID != "act_1" {
		t.Errorf("确认卡片应记在那次结果上：%+v", record.History[0].Messages)
	}
	if said := record.History[2].Messages[0]; said.Role != brain.RoleUser || said.Text != "确认" {
		t.Errorf("点了确认记成主人说的「确认」：%+v", said)
	}
}

// 点卡片上的取消：作废那次操作，不执行。
func TestTextSessionCancelFromCard(t *testing.T) {
	book := brain.NewConversationBook(nil, brain.Options{MaxHistoryTurn: 6}, quiet)
	model := braintest.NewModel(t, deleteNoteThree, braintest.Say("要删掉吗？"), braintest.Say("好，不删了。"))
	toolbox := &fakeToolbox{}
	for _, input := range []brain.TextInput{
		{Text: "删掉第 3 条", Person: "主人", Language: "zh"},
		{Person: "主人", Language: "zh", Decision: &brain.Decision{ActionID: "act_1"}},
	} {
		session := brain.NewSession(brain.Components{Front: withTestSteps(brain.Agent{Model: model}), Tools: toolbox, Thread: "t1",
			TextOnly: true, Activity: &activityRecorder{}, Conversations: book}, brain.Options{MaxHistoryTurn: 6}, quiet)
		t.Cleanup(func() { _ = session.Close() })
		textInput(t, session, input)
	}
	if len(toolbox.called()) != 0 || book.ActionOpen("主人", "t1", "act_1") {
		t.Errorf("取消了就不执行、卡片也不能再点：%v", toolbox.called())
	}
	if cancelled := toolResultOf(t, model.Requests()[2]); cancelled["status"] != "cancelled" {
		t.Errorf("作废的结果应交给模型去说：%v", cancelled)
	}
}

// 工具给人看的说明：配了的按语言填上参数（没给的参数连同冒号去掉）；没配的用工具声明的标题，再没有就是通用的一句。
func TestToolLabel(t *testing.T) {
	phrases, err := brain.LoadPhrases("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		language string
		call     brain.ToolCall
		title    string
		want     string
	}{
		{"zh", brain.ToolCall{Name: "web_search", Arguments: map[string]any{"query": "横滨 天气"}}, "", "搜索：横滨 天气"},
		{"zh", brain.ToolCall{Name: "get_weather"}, "", "查天气"},
		{"ja", brain.ToolCall{Name: "web_search", Arguments: map[string]any{"query": "横浜"}}, "", "検索：横浜"},
		{"ko", brain.ToolCall{Name: "list_calendar"}, "", "查日程"}, // 没有这种语言：用默认语言
		{"zh", brain.ToolCall{Name: "cluster_status"}, "集群状态", "集群状态"},
		{"en", brain.ToolCall{Name: "cluster_status"}, "", "Using a tool"},
	}
	for _, c := range cases {
		if got := phrases.ToolLabel(c.language, c.call, c.title); got != c.want {
			t.Errorf("%s %s：%q，应为 %q", c.language, c.call.Name, got, c.want)
		}
	}
}

// researchToolbox 的 start_research 像派调查的工具一样报告一张卡片（打字会话里才收得到）。
type researchToolbox struct{ sawCards bool }

func (r *researchToolbox) Specs() []brain.ToolSpec {
	return []brain.ToolSpec{{Name: "start_research", Parameters: json.RawMessage(`{}`)}}
}

func (r *researchToolbox) Call(ctx context.Context, _ brain.ToolCall) (string, error) {
	r.sawCards = brain.ShowsCards(ctx)
	brain.ShowCard(ctx, brain.Card{Kind: brain.CardResearch, TaskID: 7, Question: "A 和 B 哪个好"})
	return `{"id":7}`, nil
}

// 工具执行时报告的卡片：发给 App，也记进历史里那次结果上（重新打开对话还在）。语音会话里工具知道没有卡片可出。
func TestToolReportedCard(t *testing.T) {
	store, err := conversationstore.OpenSQLite(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	book := brain.NewConversationBook(store, brain.Options{MaxHistoryTurn: 6}, quiet)
	model := braintest.NewModel(t, braintest.Call("start_research", map[string]any{"question": "A 和 B 哪个好"}), braintest.Say("开始查了。"))
	toolbox, activity := &researchToolbox{}, &activityRecorder{}
	session := brain.NewSession(brain.Components{Front: withTestSteps(brain.Agent{Model: model}), Tools: toolbox, Thread: "t1",
		TextOnly: true, Activity: activity, Conversations: book}, brain.Options{MaxHistoryTurn: 6}, quiet)
	t.Cleanup(func() { _ = session.Close() })
	textInput(t, session, brain.TextInput{Text: "帮我查查 A 和 B 哪个好", Person: "主人", Language: "zh"})
	events, cards := activity.snapshot()
	if !toolbox.sawCards || len(cards) != 1 || cards[0].TaskID != 7 || !strings.HasSuffix(events[0], "派后台调查：A 和 B 哪个好") {
		t.Fatalf("卡片和工具说明：%v %+v", events, cards)
	}
	record, _, _ := store.LoadConversation("主人", "t1")
	found := false
	for _, message := range record.History[0].Messages {
		found = found || message.Card != nil && message.Card.TaskID == 7
	}
	if !found {
		t.Errorf("卡片应记进历史：%+v", record.History[0].Messages)
	}

	voice := &researchToolbox{}
	k := newToolTalker(t, braintest.NewModel(t, braintest.Call("start_research", map[string]any{}), braintest.Say("好。")), voice)
	k.say("帮我查查")
	if voice.sawCards {
		t.Error("语音会话里不该有卡片")
	}
}
