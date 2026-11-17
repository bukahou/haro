package research

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// scriptedModel 按 replies 依次回答；replies 用完后重复 repeat（nil = 给出结论）。记下每次是否给了工具。
type scriptedModel struct {
	mu        sync.Mutex
	replies   []brain.ReplyDelta
	repeat    *brain.ReplyDelta
	delay     time.Duration
	toolsSeen []bool
	lastMsgs  []brain.ChatMessage
}

func (m *scriptedModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, specs []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.mu.Lock()
	m.toolsSeen = append(m.toolsSeen, len(specs) > 0)
	m.lastMsgs = messages
	reply := brain.ReplyDelta{Text: "结论：A 比 B 便宜。\n来源：https://example.com/a"}
	switch {
	case len(specs) == 0:
		reply = brain.ReplyDelta{Text: "目前查到 A 便宜一些，B 没查完。\n来源：https://example.com/a"}
	case len(m.replies) > 0:
		reply, m.replies = m.replies[0], m.replies[1:]
	case m.repeat != nil:
		reply = *m.repeat
	}
	delay := m.delay
	m.mu.Unlock()
	return func(yield func(brain.ReplyDelta, error) bool) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				yield(brain.ReplyDelta{}, ctx.Err())
				return
			}
		}
		reply.Usage = &brain.ReplyUsage{PromptTokens: 1000, OutputTokens: 100}
		yield(reply, nil)
	}
}

func (m *scriptedModel) Warmup(context.Context) error { return nil }

type fakeSearcher struct{ searches, reads int }

func (f *fakeSearcher) Search(context.Context, string, int) (string, error) {
	f.searches++
	return "Title: A 的价格\nURL: https://example.com/a\nContent: A 100 円", nil
}
func (f *fakeSearcher) ReadPages(context.Context, []string, int, string) (string, error) {
	f.reads++
	return "A 的详细价格 100 円", nil
}
func (f *fakeSearcher) SearchCredits() int        { return 1 }
func (f *fakeSearcher) ReadCredits(pages int) int { return 1 }

type fakeBudget struct {
	mu      sync.Mutex
	refuse  error
	credits int
}

func (b *fakeBudget) AllowResearch(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refuse
}
func (b *fakeBudget) AllowSearch(context.Context, int) error {
	return b.AllowResearch(context.Background())
}
func (b *fakeBudget) RecordSearch(_ context.Context, credits int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.credits += credits
	return nil
}

func call(name string, arguments map[string]any) brain.ReplyDelta {
	return brain.ReplyDelta{ToolCalls: []brain.ToolCall{{Name: name, Arguments: arguments}}}
}

func newTestRunner(t *testing.T, model brain.ChatModel, limits Limits) (*Runner, TaskStore, *fakeSearcher, *fakeBudget) {
	t.Helper()
	store, err := OpenTaskStore(context.Background(), filepath.Join(t.TempDir(), "research.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	searcher, budget := &fakeSearcher{}, &fakeBudget{}
	return NewRunner(store, model, searcher, budget, budget, limits, quietLogger), store, searcher, budget
}

// waitFinished 等任务结束。
func waitFinished(t *testing.T, store TaskStore, id int64) Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := store.Get(context.Background(), id)
		if err == nil && task.Status.Finished() {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("任务 #%d 没结束", id)
	return Task{}
}

func TestResearchFindsAnswerAndBriefsOnce(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{
		call("search_web", map[string]any{"query": "A B 价格"}),
		call("read_pages", map[string]any{"urls": []any{"https://example.com/a"}}),
	}}
	runner, store, searcher, budget := newTestRunner(t, model, DefaultLimits)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)

	task, ahead, err := runner.Start(ctx, "主人", "A 和 B 哪个便宜")
	if err != nil || ahead != 0 {
		t.Fatalf("派任务：%v ahead=%d", err, ahead)
	}
	if lines := runner.Brief("主人"); len(lines) != 1 || !strings.Contains(lines[0], "不用主动提") {
		// 可能已经查完了：那就是结论那一行
		if len(lines) != 1 || !strings.Contains(lines[0], "查完了") {
			t.Errorf("任务板应列出在查的任务：%v", lines)
		}
	}
	done := waitFinished(t, store, task.ID)
	if done.Status != StatusDone || done.Conclusion != "结论：A 比 B 便宜。" || len(done.Sources) != 1 {
		t.Fatalf("结论：%+v", done)
	}
	if searcher.searches != 1 || searcher.reads != 1 || budget.credits != 2 {
		t.Errorf("搜索 %d、读 %d、记账 %d 积分", searcher.searches, searcher.reads, budget.credits)
	}
	lines := runner.Brief("主人")
	if !strings.Contains(strings.Join(lines, ""), "A 比 B 便宜") {
		t.Errorf("任务板应带上没告诉过的结论：%v", lines)
	}
	if again := runner.Brief("主人"); len(again) != 0 {
		t.Errorf("结论只放一次：%v", again)
	}
	if others := runner.Brief("别人"); len(others) != 0 {
		t.Errorf("不给别人看：%v", others)
	}
	if found, _ := runner.status(ctx, "主人", "便宜"); !strings.Contains(found, "A 比 B 便宜") {
		t.Errorf("按关键词找以前的调查：%s", found)
	}
}

func TestResearchStopsAtLimitsAndWritesUp(t *testing.T) {
	search := call("search_web", map[string]any{"query": "没完没了"})
	model := &scriptedModel{repeat: &search} // 模型一直想搜
	limits := DefaultLimits
	limits.MaxSearches, limits.MaxRounds = 2, 4
	runner, store, searcher, _ := newTestRunner(t, model, limits)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)
	task, _, _ := runner.Start(ctx, "主人", "无底洞")
	done := waitFinished(t, store, task.ID)

	if done.Status != StatusPartial || !strings.Contains(done.StopReason, "4 轮") || !strings.Contains(done.Conclusion, "没查完") {
		t.Errorf("到轮数上限应收工写结论：%+v", done)
	}
	if searcher.searches != 2 {
		t.Errorf("搜索次数应被截在 2 次：%d", searcher.searches)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.toolsSeen) != 5 || model.toolsSeen[4] {
		t.Errorf("4 轮带工具 + 最后一轮不带工具：%v", model.toolsSeen)
	}
}

func TestResearchTimeLimit(t *testing.T) {
	search := call("search_web", map[string]any{"query": "慢"})
	model := &scriptedModel{repeat: &search, delay: 30 * time.Millisecond}
	limits := DefaultLimits
	limits.Timeout, limits.MaxRounds = 80*time.Millisecond, 100
	runner, store, _, _ := newTestRunner(t, model, limits)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)
	task, _, _ := runner.Start(ctx, "主人", "慢慢查")
	done := waitFinished(t, store, task.ID)
	if done.Status != StatusPartial || !strings.Contains(done.StopReason, "时间上限") {
		t.Errorf("到时间上限应收工：%+v", done)
	}
}

func TestResearchBudgetQueueCancelAndRestart(t *testing.T) {
	search := call("search_web", map[string]any{"query": "慢"})
	model := &scriptedModel{repeat: &search, delay: 50 * time.Millisecond}
	limits := DefaultLimits
	limits.MaxPending, limits.MaxRounds = 2, 1000
	runner, store, _, budget := newTestRunner(t, model, limits)
	ctx, cancel := context.WithCancel(context.Background())
	go runner.Run(ctx)

	budget.refuse = errors.New("今天的调查额度用完了")
	if _, _, err := runner.Start(ctx, "主人", "x"); err == nil {
		t.Fatal("额度用完不应派出")
	}
	budget.refuse = nil
	first, _, err := runner.Start(ctx, "主人", "第一个")
	if err != nil {
		t.Fatal(err)
	}
	second, ahead, err := runner.Start(ctx, "主人", "第二个")
	if err != nil || ahead != 1 {
		t.Fatalf("第二个应排队：%v ahead=%d", err, ahead)
	}
	if _, _, err := runner.Start(ctx, "主人", "第三个"); err == nil {
		t.Error("超过同时上限应拒绝")
	}
	if _, err := runner.Cancel(ctx, "别人", first.ID); err == nil {
		t.Error("不能取消别人的调查")
	}
	time.Sleep(20 * time.Millisecond)
	cancelled, err := runner.Cancel(ctx, "主人", first.ID)
	if err != nil || cancelled.Status != StatusCancelled {
		t.Fatalf("取消在查的：%+v %v", cancelled, err)
	}

	// 服务端停掉：第二个没查完；重启后标成被中断，并在任务板上告诉主人
	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(80 * time.Millisecond)
	restarted := NewRunner(store, model, &fakeSearcher{}, budget, budget, limits, quietLogger)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go restarted.Run(ctx2)
	interrupted := waitFinished(t, store, second.ID)
	if interrupted.Status != StatusInterrupted {
		t.Fatalf("重启后应标成被中断：%+v", interrupted)
	}
	if lines := restarted.Brief("主人"); !strings.Contains(strings.Join(lines, ""), "被中断") {
		t.Errorf("任务板应告诉主人被中断了：%v", lines)
	}
}

func TestSplitSources(t *testing.T) {
	conclusion, sources := splitSources("A 便宜。\n来源：https://a.example/x、https://b.example/y")
	if conclusion != "A 便宜。" || len(sources) != 2 || sources[1] != "https://b.example/y" {
		t.Errorf("%q %v", conclusion, sources)
	}
	if conclusion, sources := splitSources("没有来源"); conclusion != "没有来源" || sources != nil {
		t.Errorf("%q %v", conclusion, sources)
	}
}

// 一次都没搜就想交差：退回去要求先搜（2026-11-17 真机：云端模型凭印象 1 秒给了结论）。
func TestResearchMustSearchBeforeConcluding(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{
		{Text: "凭印象的结论。\n来源：https://example.com/x"}, // 第一轮就想交差
		call("search_web", map[string]any{"query": "真的去搜"}),
	}}
	runner, store, searcher, _ := newTestRunner(t, model, DefaultLimits)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)
	task, _, _ := runner.Start(ctx, "主人", "要查的事")
	done := waitFinished(t, store, task.ID)
	if done.Status != StatusDone || searcher.searches != 1 || done.Conclusion != "结论：A 比 B 便宜。" {
		t.Errorf("应先被退回去搜一次，再用搜到的下结论：%+v 搜了 %d 次", done, searcher.searches)
	}
}

// 查完通知（让终端响一声）；主人自己取消的不通知。
func TestResearchNotifiesWhenFinished(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "x"})}}
	runner, store, _, _ := newTestRunner(t, model, DefaultLimits)
	finished := make(chan Task, 4)
	runner.OnFinished(func(task Task) { finished <- task })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)
	task, _, _ := runner.Start(ctx, "主人", "要查的事")
	select {
	case got := <-finished:
		if got.ID != task.ID || got.Person != "主人" || got.Status != StatusDone {
			t.Errorf("通知的任务不对：%+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("查完没有通知")
	}
	waitFinished(t, store, task.ID)

	slow := call("search_web", map[string]any{"query": "慢"})
	model.mu.Lock()
	model.repeat, model.delay = &slow, 50*time.Millisecond
	model.mu.Unlock()
	second, _, _ := runner.Start(ctx, "主人", "会被取消的")
	time.Sleep(20 * time.Millisecond)
	if _, err := runner.Cancel(ctx, "主人", second.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-finished:
		t.Errorf("取消的不该通知：%+v", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestResearchPromptHasToday(t *testing.T) {
	prompt := researchPrompt(DefaultLimits, time.Date(2026, 11, 17, 12, 0, 0, 0, time.Local))
	if !strings.Contains(prompt, "今天是 2026年11月17日（星期二）") {
		t.Errorf("调查的提示词要有今天的日期：%.80s", prompt)
	}
}
