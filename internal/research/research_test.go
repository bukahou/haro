package research

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// deliveries 收下工具登记的「主人听完才算数」的事（代替会话里的这一轮记录）。
type deliveries []func()

func (d *deliveries) Add(delivered func()) { *d = append(*d, delivered) }

// scriptedModel 按 replies 依次回答；replies 用完后重复 repeat（nil = 给出结论）。记下每次是否给了工具。
// gate 不为 nil 时，每次回答前都等测试关掉它（由测试决定调查什么时候往下走，不靠时间）。
type scriptedModel struct {
	mu        sync.Mutex
	replies   []brain.ReplyDelta
	repeat    *brain.ReplyDelta
	delay     time.Duration
	gate      chan struct{}
	toolsSeen []bool // 每次请求有没有给搜索工具（到上限后只给交结论的工具）
	lastMsgs  []brain.ChatMessage
}

// submit 是用 submit_conclusion 交出结论的回答。
func submit(conclusion string, sources ...string) brain.ReplyDelta {
	arguments := map[string]any{"conclusion": conclusion}
	if len(sources) > 0 {
		arguments["sources"] = sources
	}
	return call("submit_conclusion", arguments)
}

// offers 判断这次请求给没给某个工具。
func offers(specs []brain.ToolSpec, name string) bool {
	return slices.ContainsFunc(specs, func(spec brain.ToolSpec) bool { return spec.Name == name })
}

func (m *scriptedModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	messages := request.Messages
	specs := request.Tools
	m.mu.Lock()
	m.toolsSeen = append(m.toolsSeen, offers(specs, "search_web"))
	m.lastMsgs = messages
	reply := submit("结论：A 比 B 便宜。", "https://example.com/a")
	switch {
	case !offers(specs, "search_web"): // 到上限了，只让交结论
		reply = submit("目前查到 A 便宜一些，B 没查完。", "https://example.com/a")
	case len(m.replies) > 0:
		reply, m.replies = m.replies[0], m.replies[1:]
	case m.repeat != nil:
		reply = *m.repeat
	}
	delay, gate := m.delay, m.gate
	m.mu.Unlock()
	return func(yield func(brain.ReplyDelta, error) bool) {
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				yield(brain.ReplyDelta{}, ctx.Err())
				return
			}
		}
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
	}, gate: make(chan struct{})}
	runner, store, searcher, budget := newTestRunner(t, model, DefaultLimits)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)

	task, ahead, err := runner.Start(ctx, "主人", "A 和 B 哪个便宜")
	if err != nil || ahead != 0 {
		t.Fatalf("派任务：%v ahead=%d", err, ahead)
	}
	// 调查还卡在模型那里（gate 没开）：任务板只列出在查的这一条
	if lines := runner.Brief("主人").Lines; len(lines) != 1 || !strings.Contains(lines[0], "不用主动提") {
		t.Errorf("任务板应只列出在查的任务：%v", lines)
	}
	close(model.gate)
	done := waitFinished(t, store, task.ID)
	if done.Status != StatusDone || done.Conclusion != "结论：A 比 B 便宜。" || len(done.Sources) != 1 {
		t.Fatalf("结论：%+v", done)
	}
	if searcher.searches != 1 || searcher.reads != 1 || budget.credits != 2 {
		t.Errorf("搜索 %d、读 %d、记账 %d 积分", searcher.searches, searcher.reads, budget.credits)
	}
	// 任务板只说查完了、让模型这一轮用 research_status 取结论：结论不放在任务板上（模型不取就讲不出来）
	briefing := runner.Brief("主人")
	if board := strings.Join(briefing.Lines, ""); !strings.Contains(board, "查完了") || !strings.Contains(board, "research_status") ||
		strings.Contains(board, "A 比 B 便宜") || briefing.Delivered != nil {
		t.Errorf("任务板应只说查完了、让模型取结论，不带结论、不靠这一轮说完就算告诉了：%+v", briefing)
	}
	// 不在一轮回答里查（没有收集处）：不算告诉了；按编号查到那一个，别人的编号查不到
	if found, err := runner.status(ctx, "主人", &task.ID); err != nil || len(found.Tasks) != 1 || found.Tasks[0].Conclusion != "结论：A 比 B 便宜。" {
		t.Errorf("按编号查：%+v %v", found, err)
	}
	if found, err := runner.status(ctx, "别人", &task.ID); err != nil || len(found.Tasks) != 0 || found.Note == "" {
		t.Errorf("别人的编号查不到：%+v %v", found, err)
	}
	// 一轮回答里取了结论：登记「主人听完才算告诉了」；还没确认听完之前，任务板照样附上
	var heard deliveries
	if found, err := runner.status(brain.WithDeliveries(ctx, &heard), "主人", nil); err != nil || len(found.Tasks) != 1 ||
		found.Tasks[0].Conclusion != "结论：A 比 B 便宜。" || len(heard) != 1 {
		t.Fatalf("取结论：应带上结论，并登记一件听完才算数的事（%d 件）：%+v %v", len(heard), found, err)
	}
	if again := runner.Brief("主人"); !strings.Contains(strings.Join(again.Lines, ""), "查完了") {
		t.Errorf("主人还没听完（被打断、出错）：应一直附上：%v", again.Lines)
	}
	heard[0]()
	if again := runner.Brief("主人"); len(again.Lines) != 0 {
		t.Errorf("主人听完之后不再附上：%v", again.Lines)
	}
	if others := runner.Brief("别人"); len(others.Lines) != 0 {
		t.Errorf("不给别人看：%v", others.Lines)
	}
	var again deliveries
	if _, err := runner.status(brain.WithDeliveries(ctx, &again), "主人", nil); err != nil || len(again) != 0 {
		t.Errorf("已经告诉过的结论再取一次：不用再登记（%d 件，%v）", len(again), err)
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
		t.Errorf("4 轮带搜索工具 + 最后一轮只让交结论：%v", model.toolsSeen)
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
	if lines := restarted.Brief("主人").Lines; !strings.Contains(strings.Join(lines, ""), "被中断") {
		t.Errorf("任务板应告诉主人被中断了：%v", lines)
	}
}

// 结论和来源由模型用 submit_conclusion 分开交出；模型没用工具、直接写了文字时整段当结论（代码不从文字里切来源）。
func TestConclusionAndSourcesComeSeparately(t *testing.T) {
	for _, c := range []struct {
		name    string
		final   brain.ReplyDelta
		want    string
		sources []string
	}{
		{"用工具交", submit("A 便宜，正文里也写了来源：两家店的报价。", "https://a.example/x", "https://b.example/y"),
			"A 便宜，正文里也写了来源：两家店的报价。", []string{"https://a.example/x", "https://b.example/y"}},
		{"直接写了文字", brain.ReplyDelta{Text: "A 便宜。\n来源：https://a.example/x"}, "A 便宜。\n来源：https://a.example/x", nil},
	} {
		model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A B"}), c.final}}
		runner, store, _, _ := newTestRunner(t, model, DefaultLimits)
		ctx, cancel := context.WithCancel(context.Background())
		go runner.Run(ctx)
		task, _, _ := runner.Start(ctx, "主人", "A 和 B 哪个便宜")
		done := waitFinished(t, store, task.ID)
		cancel()
		if done.Status != StatusDone || done.Conclusion != c.want || !slices.Equal(done.Sources, c.sources) {
			t.Errorf("%s：%+v", c.name, done)
		}
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

// writerModel 是写结论的模型：回固定的文字或错误，记下收到的消息。
type writerModel struct {
	mu       sync.Mutex
	text     string
	err      error
	calls    int
	lastMsgs []brain.ChatMessage
	offered  []string // 最后一次给的工具
}

func (m *writerModel) StreamReply(_ context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	messages := request.Messages
	specs := request.Tools
	m.mu.Lock()
	m.calls++
	m.lastMsgs, m.offered = messages, nil
	for _, spec := range specs {
		m.offered = append(m.offered, spec.Name)
	}
	text, err := m.text, m.err
	m.mu.Unlock()
	return func(yield func(brain.ReplyDelta, error) bool) {
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		reply := submit(text, "https://example.com/b")
		reply.Usage = &brain.ReplyUsage{PromptTokens: 2000, OutputTokens: 300}
		yield(reply, nil)
	}
}

func (m *writerModel) Warmup(context.Context) error { return nil }

// 调查模型搜完，结论交给写结论的模型重写：不带调查模型的草稿、不给工具；写结论的模型出错时用草稿。
func TestResearchWriterWritesConclusion(t *testing.T) {
	for _, c := range []struct {
		name   string
		writer *writerModel
		want   string
	}{
		{"写结论的模型写成", &writerModel{text: "B 其实更划算。"}, "B 其实更划算。"},
		{"写结论的模型出错：用草稿", &writerModel{err: errors.New("overloaded")}, "结论：A 比 B 便宜。"},
	} {
		model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A B 价格"})}}
		runner, store, _, _ := newTestRunner(t, model, DefaultLimits)
		runner.WriteConclusionsWith(c.writer)
		ctx, cancel := context.WithCancel(context.Background())
		go runner.Run(ctx)
		task, _, _ := runner.Start(ctx, "主人", "A 和 B 哪个便宜")
		done := waitFinished(t, store, task.ID)
		cancel()
		if done.Status != StatusDone || done.Conclusion != c.want {
			t.Errorf("%s：%+v", c.name, done)
		}
		c.writer.mu.Lock()
		last := c.writer.lastMsgs[len(c.writer.lastMsgs)-1]
		for _, message := range c.writer.lastMsgs {
			if strings.Contains(message.Text, "A 比 B 便宜") {
				t.Errorf("%s：交给写结论的模型的资料里不该有调查模型的草稿", c.name)
			}
		}
		if c.writer.calls != 1 || !slices.Equal(c.writer.offered, []string{"submit_conclusion"}) || !strings.Contains(last.Text, "不要再搜索") {
			t.Errorf("%s：写结论的模型应被问 1 次、只给交结论的工具：%d 次、工具 %v、最后一条 %q", c.name, c.writer.calls, c.writer.offered, last.Text)
		}
		c.writer.mu.Unlock()
	}
}

// 到上限时也由写结论的模型写；它出错时由调查模型写。
func TestResearchWriterAtLimits(t *testing.T) {
	for _, c := range []struct {
		name   string
		writer *writerModel
		want   string
	}{
		{"写结论的模型写成", &writerModel{text: "查到一半：A 便宜。"}, "查到一半：A 便宜。"},
		{"写结论的模型出错：调查模型写", &writerModel{err: errors.New("overloaded")}, "目前查到 A 便宜一些，B 没查完。"},
	} {
		search := call("search_web", map[string]any{"query": "没完没了"})
		model := &scriptedModel{repeat: &search}
		limits := DefaultLimits
		limits.MaxSearches, limits.MaxRounds = 1, 2
		runner, store, _, _ := newTestRunner(t, model, limits)
		runner.WriteConclusionsWith(c.writer)
		ctx, cancel := context.WithCancel(context.Background())
		go runner.Run(ctx)
		task, _, _ := runner.Start(ctx, "主人", "无底洞")
		done := waitFinished(t, store, task.ID)
		cancel()
		if done.Status != StatusPartial || done.Conclusion != c.want {
			t.Errorf("%s：%+v", c.name, done)
		}
	}
}

// 任务板一次读出这个人在查的、查完还没说的：查完说过的、取消的、别人的都不在里面。
func TestTaskStoreBoard(t *testing.T) {
	ctx := context.Background()
	store, err := OpenTaskStore(ctx, filepath.Join(t.TempDir(), "research.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	create := func(person, question string) int64 {
		task, err := store.Create(ctx, person, question)
		if err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	queued := create("主人", "排队中")
	running := create("主人", "在查")
	_ = store.MarkRunning(ctx, running)
	unreported := create("主人", "查完没说")
	_, _ = store.Finish(ctx, unreported, StatusDone, "结论", nil, "")
	reported := create("主人", "查完说过")
	_, _ = store.Finish(ctx, reported, StatusDone, "结论", nil, "")
	_ = store.MarkReported(ctx, reported)
	cancelled := create("主人", "取消了")
	_, _ = store.Finish(ctx, cancelled, StatusCancelled, "", nil, "")
	create("别人", "别人的")

	board, err := store.Board(ctx, "主人")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, task := range board {
		ids = append(ids, task.ID)
	}
	if want := []int64{queued, running, unreported}; !slices.Equal(ids, want) {
		t.Errorf("任务板应是排队中、在查、查完没说的（按派出顺序）：得到 %v，应为 %v", ids, want)
	}
}

// 取消和查完同时到：先结束的算数，后到的不改它（以前「查完」会盖掉已经记下的「取消」，或反过来）。
func TestFinishOnlyOnce(t *testing.T) {
	ctx := context.Background()
	store, err := OpenTaskStore(ctx, filepath.Join(t.TempDir(), "research.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.Create(ctx, "主人", "A 和 B 哪个便宜")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.MarkRunning(ctx, task.ID)
	if cancelled, err := store.Finish(ctx, task.ID, StatusCancelled, "", nil, "主人说不查了"); err != nil || !cancelled {
		t.Fatalf("在查的任务应能取消：%v %v", cancelled, err)
	}
	if finished, err := store.Finish(ctx, task.ID, StatusDone, "结论：A 便宜。", nil, ""); err != nil || finished {
		t.Errorf("已经取消的任务不应再被「查完」改掉：%v %v", finished, err)
	}
	if got, _ := store.Get(ctx, task.ID); got.Status != StatusCancelled || got.Conclusion != "" {
		t.Errorf("应保持取消：%+v", got)
	}
}
