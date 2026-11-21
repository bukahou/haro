package brain_test

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

// interruptWhen 在 ready 成立时（例如模型收到了第 n 次请求）像终端那样打断：先报播到哪，再取消回答。
func interruptWhen(t *testing.T, k *talker, ready func() bool, playedMs int64) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(braintest.EventTimeout)
		for !ready() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		_ = k.session.Send(context.Background(), realtimeproto.ConversationItemTruncate{AudioEndMs: playedMs})
		_ = k.session.Send(context.Background(), realtimeproto.ResponseCancel{})
	}()
}

// roles 是消息的角色序列（看历史的形状用）。
func roles(messages []brain.ChatMessage) []brain.ChatRole {
	var out []brain.ChatRole
	for _, msg := range messages {
		out = append(out, msg.Role)
	}
	return out
}

// 调用工具前说的话只记在那条工具调用的消息里，最后一条回答只记最后一段（以前整轮的文字拼在一起记，开头那句重复两遍）。
func TestTurnRecordsEachPartOnce(t *testing.T) {
	lookup := braintest.Reply{Chunks: []string{"我查一下。"}, ToolCalls: searchCarSpot.ToolCalls}
	model := braintest.NewModel(t, lookup, braintest.Say("车在C3的7号位。"), braintest.Say("不客气。"))
	k := newToolTalker(t, model, &fakeToolbox{})

	if spoken := k.say("我的车停在哪了？"); spoken != "我查一下。车在C3的7号位。" {
		t.Fatalf("播出 %q", spoken)
	}
	k.say("谢谢")
	history := withoutSynthetic(model.LastRequest().Messages)
	want := []brain.ChatRole{brain.RoleUser, brain.RoleAssistant, brain.RoleTool, brain.RoleAssistant, brain.RoleUser}
	if got := roles(history); !slices.Equal(got, want) {
		t.Fatalf("历史的形状 %v，应为 %v", got, want)
	}
	if history[1].Text != "我查一下。" || len(history[1].ToolCalls) != 1 || history[3].Text != "车在C3的7号位。" {
		t.Errorf("调用前的话应只在工具调用那条里，最后的回答只有最后一段：%q / %q", history[1].Text, history[3].Text)
	}
}

// 工具做完、还没说出结论就被打断：历史里有这次工具调用和结果，模型下一轮知道自己查过。
func TestToolDoneThenInterruptedIsRemembered(t *testing.T) {
	lookup := braintest.Reply{Chunks: []string{"我查一下。"}, ToolCalls: searchCarSpot.ToolCalls}
	model := braintest.NewModel(t, lookup, braintest.Reply{Chunks: []string{"查到了"}, Hang: true}, braintest.Say("查过了，在C3。"))
	toolbox := &fakeToolbox{}
	k := newToolTalker(t, model, toolbox)

	interruptWhen(t, k, func() bool { return len(model.Requests()) >= 2 }, 60_000) // 说结论之前打断（「查到了」还没说出口）
	if done := braintest.Done(t, k.ask("我的车停在哪了？")); done.Status != realtimeproto.StatusCancelled {
		t.Fatalf("应是被打断：%+v", done)
	}
	k.say("刚才查到了吗？")
	history := withoutSynthetic(model.LastRequest().Messages)
	want := []brain.ChatRole{brain.RoleUser, brain.RoleAssistant, brain.RoleTool, brain.RoleAssistant, brain.RoleUser}
	if got := roles(history); !slices.Equal(got, want) {
		t.Fatalf("历史的形状 %v，应为 %v（被打断的那一轮要留下工具往来）", got, want)
	}
	if history[2].Text != "车停在C3的7号位" || history[3].Text != brain.InterruptedBeforeSpeaking {
		t.Errorf("工具结果应照实记、回答记成没说出口：%q / %q", history[2].Text, history[3].Text)
	}
}

// slowToolbox：slow_tool 一直执行到被取消；其余照 fakeToolbox。
type slowToolbox struct {
	fakeToolbox
	started chan struct{}
	once    sync.Once
}

func (b *slowToolbox) Call(ctx context.Context, call brain.ToolCall) (string, error) {
	if call.Name != "slow_tool" {
		return b.fakeToolbox.Call(ctx, call)
	}
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return "", ctx.Err()
}

// 一次要求了两个调用、第一个执行中被打断：第一个照实记（出错），第二个没有执行，补「没有执行」——历史里没有落单的调用。
func TestInterruptedDuringToolsLeavesNoDanglingCall(t *testing.T) {
	both := braintest.Reply{ToolCalls: []brain.ToolCall{{Name: "slow_tool"}, {Name: "search_notes"}}}
	model := braintest.NewModel(t, both, braintest.Say("好的。"))
	toolbox := &slowToolbox{started: make(chan struct{})}
	k := newToolTalker(t, model, toolbox)

	interruptWhen(t, k, func() bool {
		select {
		case <-toolbox.started:
			return true
		default:
			return false
		}
	}, 0)
	braintest.Done(t, k.ask("查一下"))
	if got := toolbox.called(); len(got) != 0 {
		t.Errorf("被打断后不应再执行后面的调用：%v", got)
	}
	k.say("算了")
	history := withoutSynthetic(model.LastRequest().Messages)
	var results []string
	for _, msg := range history {
		if msg.Role == brain.RoleTool {
			results = append(results, msg.Text)
		}
	}
	if len(results) != 2 || !strings.Contains(results[0], "工具执行失败") || results[1] != brain.ToolNotRunInterrupted {
		t.Errorf("两个调用都应有结果（执行中的照实记、没执行的补说明）：%q", results)
	}
}

// 工具做完后模型出错：这一轮照样记下工具往来，回答记成出错没说出来；response.done 是失败。
func TestToolDoneThenModelFailsIsRemembered(t *testing.T) {
	model := braintest.NewModel(t, searchCarSpot, braintest.Reply{Err: errors.New("503")}, braintest.Say("在C3。"))
	k := newToolTalker(t, model, &fakeToolbox{})

	if done := braintest.Done(t, k.ask("我的车停在哪了？")); done.Status != realtimeproto.StatusFailed {
		t.Fatalf("应是失败：%+v", done)
	}
	k.say("再说一遍")
	history := withoutSynthetic(model.LastRequest().Messages)
	if got := roles(history); len(got) != 5 || history[2].Text != "车停在C3的7号位" || history[3].Text != brain.FailedBeforeSpeaking {
		t.Errorf("出错的那一轮应留下工具往来，回答记成出错：%v %+v", got, history)
	}
}

// slowSynth 每句合成要 200 ms（念到一半有机会被打断），每句给 1 秒的声音。
type slowSynth struct{}

func (slowSynth) Synthesize(ctx context.Context, _ brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		select {
		case <-time.After(200 * time.Millisecond):
			yield(realtimeproto.PCMAudio{Format: testFormat, Data: make([]byte, 32000)}, nil)
		case <-ctx.Done():
			yield(realtimeproto.PCMAudio{}, ctx.Err())
		}
	}
}

// 问确认的话还没说完就被打断：主人没听完问话，这一轮存下的待确认操作作废，之后确认也不执行。
func TestInterruptedConfirmationIsWithdrawn(t *testing.T) {
	model := braintest.NewModel(t, braintest.Call("delete_note", map[string]any{"ids": []any{3}}),
		braintest.Reply{Chunks: []string{"要删掉第 3 条吗？"}, Hang: true},
		braintest.Call("confirm_action", map[string]any{"action_id": "act_1"}), braintest.Say("好的。"))
	toolbox := &fakeToolbox{}
	k := newTalker(t, brain.Components{Model: model, Synthesizer: slowSynth{}, Tools: toolbox,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(0, 40) }}, brain.Options{MaxHistoryTurn: 6})

	interruptWhen(t, k, func() bool { return len(model.Requests()) >= 2 }, 300) // 问题刚开始念就打断
	if done := braintest.Done(t, k.owner("把那条删了")); done.Status != realtimeproto.StatusCancelled {
		t.Fatalf("应是被打断：%+v", done)
	}
	k.owner("可以")
	if got := toolbox.called(); len(got) != 0 {
		t.Errorf("问确认时被打断，之后确认也不应执行：%v", got)
	}
}

// latestStore 是只记最新一份的内存存储。
type latestStore struct {
	mu      sync.Mutex
	records map[string]brain.ConversationRecord
}

func (s *latestStore) LoadConversation(person string) (brain.ConversationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[person]
	return record, ok, nil
}

func (s *latestStore) SaveConversation(record brain.ConversationRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = map[string]brain.ConversationRecord{}
	}
	s.records[record.Person] = record
	return nil
}

func (s *latestStore) openTurn(person string) *brain.ConversationTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[person].OpenTurn
}

// 工具一做完就先存一份这一轮（按「服务端重启、没说完」整理好）：模型还在生成时服务端重启，重启后能接上；这一轮收尾后清掉。
func TestToolResultIsSavedBeforeTheTurnEnds(t *testing.T) {
	store := &latestStore{}
	model := braintest.NewModel(t, searchCarSpot, braintest.Reply{Chunks: []string{"查到了"}, Hang: true})
	opts := brain.Options{MaxHistoryTurn: 6}
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{}, Tools: &fakeToolbox{},
		Conversations: brain.NewConversationBook(store, opts, quiet)}, opts)

	interruptWhen(t, k, func() bool {
		if len(model.Requests()) < 2 {
			return false
		}
		open := store.openTurn(brain.SharedPerson)
		if open == nil || !slices.ContainsFunc(open.Messages, func(msg brain.ChatMessage) bool { return msg.Text == "车停在C3的7号位" }) {
			t.Errorf("模型还在生成时，存储里应已有这一轮（带工具结果）：%+v", open)
		}
		return true
	}, 0)
	braintest.Done(t, k.ask("我的车停在哪了？"))
	if open := store.openTurn(brain.SharedPerson); open != nil {
		t.Errorf("这一轮收尾后应清掉没收尾的记录：%+v", open)
	}
}

// 关停时关会话：Close 等进行中的那一轮收尾（照被打断的规则写进历史）才返回——之后关数据库不会和它抢。
func TestCloseWaitsForTheTurnToBeRecorded(t *testing.T) {
	store := &latestStore{}
	model := braintest.NewModel(t, braintest.Reply{Chunks: []string{"从前有座山。"}, Hang: true})
	opts := brain.Options{MaxHistoryTurn: 6}
	k := newTalker(t, brain.Components{Model: model, Synthesizer: fakeSynthesizer{},
		Conversations: brain.NewConversationBook(store, opts, quiet)}, opts)

	braintest.Speak(t, k.session, k.stream, braintest.Utterance{})
	braintest.Expect[realtimeproto.ResponseCreated](t, k.session)
	braintest.Recognized(t, k.stream, braintest.Utterance{Text: "讲个故事"})
	deadline := time.Now().Add(braintest.EventTimeout)
	for len(model.Requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	k.send(realtimeproto.ConversationItemTruncate{AudioEndMs: 500}) // 终端报了播到哪
	_ = k.session.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	if history := store.records[brain.SharedPerson].History; len(history) != 1 {
		t.Errorf("Close 返回时这一轮应已写进历史：%+v", history)
	}
}
