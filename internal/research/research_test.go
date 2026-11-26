package research

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
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

// 假模型交的两种结论：查够了交的、到上限（不给搜索工具）时交的。
const (
	scriptedConclusion = "结论：A 比 B 便宜。"
	limitConclusion    = "目前查到 A 便宜一些，B 没查完。"
)

// scriptedModel 按 replies 依次回答；replies 用完后重复 repeat（nil = 交出 scriptedConclusion）；到上限时
// （这次请求不给搜索工具）交出 limitConclusion。每一步做什么由测试指定（不看消息的内容），什么时候往下走也由测试决定，不靠时间：
// gate 不为 nil 时每次回答前等它关上（或被取消）；hang 为 true 时 replies 用完后的搜索阶段请求一直等到被取消；
// 每次请求开始时往 entered 放一个（测试据此知道调查走到了模型这一步）；onRequest 是第几次请求（从 1 数）开始时要做的事。
type scriptedModel struct {
	mu        sync.Mutex
	replies   []brain.ReplyDelta
	repeat    *brain.ReplyDelta
	failures  map[int]error // 第几次请求（从 1 数）出错
	onRequest map[int]func()
	gate      chan struct{}
	hang      bool
	entered   chan struct{}
	requests  int
	toolsSeen []bool                // 每次请求有没有给搜索工具（到上限后只给交结论的工具）
	questions []string              // 每次请求的任务（系统提示词后面那条：现在的日期和要调查的问题）
	seen      [][]brain.ChatMessage // 每次请求带的消息
}

// submit 是用 submit_conclusion 交出结论的回答。
func submit(conclusion string, sources ...string) brain.ReplyDelta {
	arguments := map[string]any{"conclusion": conclusion}
	if len(sources) > 0 {
		arguments["sources"] = sources
	}
	return call("submit_conclusion", arguments)
}

func call(name string, arguments map[string]any) brain.ReplyDelta {
	return brain.ReplyDelta{ToolCalls: []brain.ToolCall{{Name: name, Arguments: arguments}}}
}

// offers 判断这次请求给没给某个工具。
func offers(specs []brain.ToolSpec, name string) bool {
	return slices.ContainsFunc(specs, func(spec brain.ToolSpec) bool { return spec.Name == name })
}

func (m *scriptedModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	m.mu.Lock()
	m.requests++
	m.toolsSeen = append(m.toolsSeen, offers(request.Tools, "search_web"))
	m.questions = append(m.questions, request.Messages[1].Text)
	m.seen = append(m.seen, slices.Clone(request.Messages))
	failure, hook := m.failures[m.requests], m.onRequest[m.requests]
	reply := submit(scriptedConclusion, "https://example.com/a")
	block := false
	switch {
	case !offers(request.Tools, "search_web"): // 到上限了，只让交结论
		reply = submit(limitConclusion, "https://example.com/a")
	case len(m.replies) > 0:
		reply, m.replies = m.replies[0], m.replies[1:]
	case m.hang:
		block = true
	case m.repeat != nil:
		reply = *m.repeat
	}
	gate, entered := m.gate, m.entered
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	return func(yield func(brain.ReplyDelta, error) bool) {
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if gate != nil || block {
			if gate == nil {
				gate = make(chan struct{}) // 一直等到被取消
			}
			select {
			case <-gate:
			case <-ctx.Done():
				yield(brain.ReplyDelta{}, ctx.Err())
				return
			}
		}
		if failure != nil {
			yield(brain.ReplyDelta{}, failure)
			return
		}
		reply.Usage, reply.Stop = &brain.ReplyUsage{PromptTokens: 1000, OutputTokens: 100}, brain.StopCompleted
		yield(reply, nil)
	}
}

// setGate 换掉（或去掉）每次回答前要等的闸。
func (m *scriptedModel) setGate(gate chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gate = gate
}

// messagesOfLastRequest 是最后一次请求带的消息。
func (m *scriptedModel) messagesOfLastRequest() []brain.ChatMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen[len(m.seen)-1]
}

// requestMessages 是每次请求带的消息。
func (m *scriptedModel) requestMessages() [][]brain.ChatMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.seen)
}

// fakeSearcher 是假的搜索服务：记下搜了几次、读了哪些网址；searchErr 不为 nil 时搜索失败。
// 每次调用算发出一次请求，用 record 记一笔（失败的标不完整）。hits 是每次搜索交回的结果（nil = 一条 A）；
// pages 是网址对应的正文（没有的交回 defaultPage）。
type fakeSearcher struct {
	mu         sync.Mutex
	searches   int
	reads      int
	readURLs   []string
	chargedFor int // 最后一次读网页按几页记账
	searchErr  error
	hits       []SearchHit
	pages      map[string]string
}

const defaultPage = "A 的详细价格 100 円"

func (f *fakeSearcher) Search(ctx context.Context, _ string, _ int, record Recorder) ([]SearchHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches++
	if err := record.RecordSearch(ctx, 1, f.searchErr != nil); err != nil {
		return nil, err
	}
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.hits != nil {
		return slices.Clone(f.hits), nil
	}
	return []SearchHit{{Title: "A 的价格", URL: "https://example.com/a", Snippet: "A 100 円"}}, nil
}

func (f *fakeSearcher) ReadPages(ctx context.Context, urls []string, _ int, _ string, record Recorder) ([]Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.readURLs = append(f.readURLs, urls...)
	if err := record.RecordSearch(ctx, 1, false); err != nil {
		return nil, err
	}
	pages := make([]Page, len(urls))
	for i, url := range urls {
		content, ok := f.pages[url]
		if !ok {
			content = defaultPage
		}
		pages[i] = Page{URL: url, Content: content}
	}
	return pages, nil
}

func (f *fakeSearcher) SearchCredits() int { return 1 }

func (f *fakeSearcher) ReadCredits(pages int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chargedFor = pages
	return 1
}

// counts 返回搜了几次、读了几次。
func (f *fakeSearcher) counts() (searches, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searches, f.reads
}

type fakeBudget struct {
	mu         sync.Mutex
	refuse     error
	credits    int
	incomplete int // 记成不完整的笔数（请求发出去了、没拿到结果）
}

func (b *fakeBudget) refuseWith(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuse = err
}

func (b *fakeBudget) AllowResearch(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refuse
}
func (b *fakeBudget) AllowSearch(context.Context, int) error {
	return b.AllowResearch(context.Background())
}
func (b *fakeBudget) RecordSearch(_ context.Context, credits int, incomplete bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.credits += credits
	if incomplete {
		b.incomplete++
	}
	return nil
}

func (b *fakeBudget) incompleteCharges() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.incomplete
}

func (b *fakeBudget) recorded() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.credits
}

// researchRig 是一套测试用的调查执行器：任务账本在临时目录，查完（没被取消）的任务从 finished 送出来。
type researchRig struct {
	runner   *Runner
	store    TaskStore
	searcher *fakeSearcher
	budget   *fakeBudget
	finished chan Task
}

func openTestStore(t *testing.T) TaskStore {
	t.Helper()
	store, err := OpenTaskStore(context.Background(), filepath.Join(t.TempDir(), "research.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// rigSetup 是测试用的调查执行器的设定：零值 = 不审查、默认上限（DefaultLimits）、testRounds 轮。
type rigSetup struct {
	reviewer      brain.ChatModel // 审查的模型；nil = 不审查
	reviewTimeout time.Duration   // 审一次最多多久；0 = 5 秒（只是防止测试卡死）
	limits        Limits          // 零值 = DefaultLimits
	rounds        int             // 调查的轮数（调查的代理的 MaxSteps）；0 = testRounds
	tools         []string        // 调查的代理的允许名单；nil = 全部
}

// testRounds 是测试里调查的轮数（和程序里带的定义一样）；testPrompt、testReviewPrompt 是占位的系统提示词。
const (
	testRounds       = 10
	testPrompt       = "（测试用的调查提示词）"
	testReviewPrompt = "（测试用的审查提示词）"
)

// agents 是按设定组装的两个代理。
func (setup rigSetup) agents(investigator brain.ChatModel) Agents {
	agents := Agents{Investigator: brain.Agent{Name: "调查", Model: investigator, Instructions: testPrompt, Tools: setup.tools,
		MaxSteps: cmp.Or(setup.rounds, testRounds)}}
	if setup.reviewer != nil {
		agents.Reviewer = &brain.Agent{Name: "调查审查", Model: setup.reviewer, Instructions: testReviewPrompt, MaxSteps: 1,
			Timeout: cmp.Or(setup.reviewTimeout, 5*time.Second)}
	}
	return agents
}

func (setup rigSetup) limitsOrDefault() Limits {
	if setup.limits == (Limits{}) {
		return DefaultLimits
	}
	return setup.limits
}

// newTestRunner 建调查执行器；建不成就让测试失败。
func newTestRunner(t *testing.T, store TaskStore, model brain.ChatModel, searcher Searcher, budget *fakeBudget, setup rigSetup) *Runner {
	t.Helper()
	runner, err := NewRunner(store, setup.agents(model), searcher, budget, budget, setup.limitsOrDefault(), quietLogger)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func newRig(t *testing.T, model brain.ChatModel, setup rigSetup) *researchRig {
	t.Helper()
	rig := &researchRig{store: openTestStore(t), searcher: &fakeSearcher{}, budget: &fakeBudget{}, finished: make(chan Task, 16)}
	rig.runner = newTestRunner(t, rig.store, model, rig.searcher, rig.budget, setup)
	rig.runner.OnFinished(func(task Task) { rig.finished <- task })
	return rig
}

// run 在后台跑执行器，直到 ctx 取消；返回的 channel 在 Run 退出后关闭。
func (rig *researchRig) run(ctx context.Context) <-chan struct{} {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		rig.runner.Run(ctx)
	}()
	return stopped
}

// waitFinished 等这个任务结束（查完、到上限收工或失败；主人取消的不会送出来）。时限只是防止测试卡死。
func (rig *researchRig) waitFinished(t *testing.T, id int64) Task {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case task := <-rig.finished:
			if task.ID == id {
				return task
			}
		case <-deadline:
			t.Fatalf("任务 #%d 没结束", id)
		}
	}
}

// research 派一个任务并等它结束。
func (rig *researchRig) research(t *testing.T, question string) Task {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rig.run(ctx)
	task, _, err := rig.runner.Start(ctx, "主人", question)
	if err != nil {
		t.Fatal(err)
	}
	return rig.waitFinished(t, task.ID)
}

// waitEntered 等模型收到下一次请求。时限只是防止测试卡死。
func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("调查没走到模型那一步")
	}
}

func TestResearchFindsAnswerAndBriefsOnce(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{
		call("search_web", map[string]any{"query": "A B 价格"}),
		call("read_pages", map[string]any{"urls": []any{"https://example.com/a"}}),
	}, gate: make(chan struct{})}
	rig := newRig(t, model, rigSetup{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.run(ctx)

	const question = "A 和 B 哪个便宜"
	task, ahead, err := rig.runner.Start(ctx, "主人", question)
	if err != nil || ahead != 0 {
		t.Fatalf("派任务：%v ahead=%d", err, ahead)
	}
	// 调查还卡在模型那里（gate 没开）：任务板只列出这一条，不用等主人听完来确认
	if board := rig.runner.Brief("主人"); len(board.Lines) != 1 || !strings.Contains(board.Lines[0], question) || board.Delivered != nil {
		t.Errorf("任务板应只列出在查的任务：%+v", board)
	}
	close(model.gate)
	done := rig.waitFinished(t, task.ID)
	if done.Status != StatusDone || done.Conclusion != scriptedConclusion || len(done.Sources) != 1 {
		t.Fatalf("结论：%+v", done)
	}
	if searches, reads := rig.searcher.counts(); searches != 1 || reads != 1 || rig.budget.recorded() != 2 {
		t.Errorf("搜索 %d、读 %d、记账 %d 积分", searches, reads, rig.budget.recorded())
	}
	// 任务板只提这个任务查完了：让模型先回答主人这句、再一句话问要不要听（结论不放在任务板上，要听再取）；
	// 主人听完这一轮就算告诉了
	briefing := rig.runner.Brief("主人")
	if len(briefing.Lines) != 1 || !strings.Contains(briefing.Lines[0], question) || strings.Contains(briefing.Lines[0], scriptedConclusion) ||
		!strings.Contains(briefing.Lines[0], "先回答主人这次说的话") || briefing.Delivered == nil {
		t.Errorf("任务板应只提查完了、先回答这句再问要不要听，听完就算告诉了：%+v", briefing)
	}
	// 不在一轮回答里查（没有收集处）：不算告诉了；按编号查到那一个，别人的编号查不到
	if found, err := rig.runner.Status(ctx, "主人", &task.ID); err != nil || len(found.Tasks) != 1 || found.Tasks[0].Conclusion != scriptedConclusion {
		t.Errorf("按编号查：%+v %v", found, err)
	}
	if found, err := rig.runner.Status(ctx, "别人", &task.ID); err != nil || len(found.Tasks) != 0 || found.Note == "" {
		t.Errorf("别人的编号查不到：%+v %v", found, err)
	}
	// 主人要听、一轮回答里取了结论：同样登记「主人听完才算告诉了」；还没确认听完之前，任务板照样附上
	var heard deliveries
	if found, err := rig.runner.Status(brain.WithDeliveries(ctx, &heard), "主人", nil); err != nil || len(found.Tasks) != 1 ||
		found.Tasks[0].Conclusion != scriptedConclusion || len(heard) != 1 {
		t.Fatalf("取结论：应带上结论，并登记一件听完才算数的事（%d 件）：%+v %v", len(heard), found, err)
	}
	if again := rig.runner.Brief("主人"); len(again.Lines) != 1 {
		t.Errorf("主人还没听完（被打断、出错）：应一直附上：%v", again.Lines)
	}
	heard[0]()
	if again := rig.runner.Brief("主人"); len(again.Lines) != 0 {
		t.Errorf("主人听完之后不再附上：%v", again.Lines)
	}
	if reported, err := rig.store.Get(ctx, task.ID); err != nil || !reported.Reported {
		t.Errorf("主人听完之后应标成已告诉：%+v %v", reported, err)
	}
	if others := rig.runner.Brief("别人"); len(others.Lines) != 0 {
		t.Errorf("不给别人看：%v", others.Lines)
	}
	var again deliveries
	if _, err := rig.runner.Status(brain.WithDeliveries(ctx, &again), "主人", nil); err != nil || len(again) != 0 {
		t.Errorf("已经告诉过的结论再取一次：不用再登记（%d 件，%v）", len(again), err)
	}
}

// 附上「查完了」的那一轮被主人打断：下一轮再附上一次（说明上次被打断了，由模型看历史判断要不要再说）；
// 又被打断就当告诉过了，不再附上（结论随时能问），任务标成已告诉。
func TestBoardNoticeIsRepeatedOnceAfterInterruption(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A B 价格"})}}
	rig := newRig(t, model, rigSetup{})
	task := rig.research(t, "A 和 B 哪个便宜")
	ctx := context.Background()

	first := rig.runner.Brief("主人")
	if len(first.Lines) != 1 || strings.Contains(first.Lines[0], brain.CutNote) || first.Interrupted == nil {
		t.Fatalf("第一次附上：%+v", first)
	}
	first.Interrupted()
	second := rig.runner.Brief("主人")
	if len(second.Lines) != 1 || !strings.Contains(second.Lines[0], brain.CutNote) {
		t.Fatalf("被打断后再附上一次，并说明上次被打断了：%+v", second)
	}
	second.Interrupted()
	if third := rig.runner.Brief("主人"); len(third.Lines) != 0 {
		t.Fatalf("又被打断就不再附上：%+v", third)
	}
	if reported, err := rig.store.Get(ctx, task.ID); err != nil || !reported.Reported {
		t.Errorf("当告诉过了：应标成已告诉：%+v %v", reported, err)
	}
	if found, err := rig.runner.Status(ctx, "主人", &task.ID); err != nil || found.Tasks[0].Conclusion == "" {
		t.Errorf("结论照样能问：%+v %v", found, err)
	}
}

func TestResearchStopsAtLimitsAndWritesUp(t *testing.T) {
	search := call("search_web", map[string]any{"query": "没完没了"})
	model := &scriptedModel{repeat: &search} // 模型一直想搜
	limits := DefaultLimits
	limits.MaxSearches = 2
	rig := newRig(t, model, rigSetup{limits: limits, rounds: 4})
	done := rig.research(t, "无底洞")

	if done.Status != StatusPartial || done.Conclusion != limitConclusion || done.StopReason == "" {
		t.Errorf("到轮数上限应收工、用查到的写结论：%+v", done)
	}
	if searches, _ := rig.searcher.counts(); searches != 2 {
		t.Errorf("搜索次数应被截在 2 次：%d", searches)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if want := []bool{true, true, true, true, false}; !slices.Equal(model.toolsSeen, want) {
		t.Errorf("4 轮带搜索工具 + 最后一轮只让交结论：%v", model.toolsSeen)
	}
}

// 到时间上限：搜索阶段的请求被打断，用已经查到的资料写结论。
func TestResearchTimeLimit(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "慢"})}, hang: true}
	limits := DefaultLimits
	limits.Timeout = 50 * time.Millisecond
	rig := newRig(t, model, rigSetup{limits: limits, rounds: 100})
	done := rig.research(t, "慢慢查")
	if done.Status != StatusPartial || done.Conclusion != limitConclusion {
		t.Errorf("到时间上限应收工、用查到的写结论：%+v", done)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if want := []bool{true, true, false}; !slices.Equal(model.toolsSeen, want) {
		t.Errorf("搜一次、等到时间上限、再只让交结论：%v", model.toolsSeen)
	}
}

func TestResearchBudgetQueueCancelAndRestart(t *testing.T) {
	search := call("search_web", map[string]any{"query": "慢"})
	model := &scriptedModel{repeat: &search, gate: make(chan struct{}), entered: make(chan struct{}, 64)} // 闸一直不开：在查的一直在查
	rig := newRig(t, model, rigSetup{})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := rig.run(ctx)

	rig.budget.refuseWith(errors.New("今天的调查额度用完了"))
	if _, _, err := rig.runner.Start(ctx, "主人", "x"); err == nil {
		t.Fatal("额度用完不应派出")
	}
	rig.budget.refuseWith(nil)
	first, _, err := rig.runner.Start(ctx, "主人", "第一个")
	if err != nil {
		t.Fatal(err)
	}
	var queued []Task // 排在第一个后面的（同时上限是 maxPending）
	for i := 1; i < maxPending; i++ {
		task, ahead, err := rig.runner.Start(ctx, "主人", "排队的 "+strconv.Itoa(i))
		if err != nil || ahead != i {
			t.Fatalf("第 %d 个应排队：%v ahead=%d", i+1, err, ahead)
		}
		queued = append(queued, task)
	}
	if _, _, err := rig.runner.Start(ctx, "主人", "超出的"); err == nil {
		t.Error("超过同时上限应拒绝")
	}
	if _, err := rig.runner.Cancel(ctx, "别人", first.ID); err == nil {
		t.Error("不能取消别人的调查")
	}
	waitEntered(t, model.entered) // 第一个在查
	cancelled, err := rig.runner.Cancel(ctx, "主人", first.ID)
	if err != nil || cancelled.Status != StatusCancelled {
		t.Fatalf("取消在查的：%+v %v", cancelled, err)
	}

	// 服务端停掉：第二个在查、后面的在排队，都没查完；重启后标成被中断，并在任务板上告诉主人（这一轮说完就算告诉了）
	waitEntered(t, model.entered) // 第二个在查
	cancel()
	<-stopped
	restarted := newTestRunner(t, rig.store, model, &fakeSearcher{}, rig.budget, rigSetup{})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go restarted.Run(ctx2)
	<-restarted.ready // 启动清理做完了
	for _, task := range queued {
		interrupted, err := rig.store.Get(ctx2, task.ID)
		if err != nil || interrupted.Status != StatusInterrupted {
			t.Fatalf("重启后应标成被中断：%+v %v", interrupted, err)
		}
	}
	board := restarted.Brief("主人")
	if len(board.Lines) != len(queued) || !strings.Contains(board.Lines[0], queued[0].Question) || board.Delivered == nil {
		t.Errorf("任务板应告诉主人被中断了，这一轮说完就算告诉了：%+v", board)
	}
}

// 排队的队列满了（排队时取消的任务还占着位置）：这次不派、不建任务（不会建了再记失败、让主人听两次）；队列空出来就又能派。
func TestStartRefusesWhenQueueIsFullWithoutCreatingTask(t *testing.T) {
	store := openTestStore(t)
	runner := newTestRunner(t, store, &scriptedModel{}, &fakeSearcher{}, &fakeBudget{}, rigSetup{})
	close(runner.ready) // 不跑 Run：队列里的不会被取走
	ctx := context.Background()
	for len(runner.queue) < cap(runner.queue) { // 派了又取消：任务都结束了，队列里的位置还占着
		task, _, err := runner.Start(ctx, "主人", "派了又取消")
		if err != nil {
			t.Fatalf("队列还有位置时应能派（队列里 %d 个）：%v", len(runner.queue), err)
		}
		if _, err := runner.Cancel(ctx, "主人", task.ID); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.Recent(ctx, "主人", queueCapacity+10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.Start(ctx, "主人", "放不进去的"); !errors.Is(err, errQueueFull) {
		t.Fatalf("队列满了应拒绝：%v", err)
	}
	after, err := store.Recent(ctx, "主人", queueCapacity+10)
	if err != nil || len(after) != len(before) {
		t.Errorf("队列满了不该建任务：之前 %d 个、之后 %d 个（%v）", len(before), len(after), err)
	}
	if board := runner.Brief("主人"); len(board.Lines) != 0 {
		t.Errorf("没派出去的不上任务板（主人已经从工具的报错听到了）：%v", board.Lines)
	}
	<-runner.queue // 取走一个（像 Run 跳过一个取消了的）：又能派
	if _, _, err := runner.Start(ctx, "主人", "空出来之后"); err != nil {
		t.Errorf("队列空出位置后应能派：%v", err)
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
		done := newRig(t, model, rigSetup{}).research(t, "A 和 B 哪个便宜")
		if done.Status != StatusDone || done.Conclusion != c.want || !slices.Equal(done.Sources, c.sources) {
			t.Errorf("%s：%+v", c.name, done)
		}
	}
}

// 一份资料都没拿到（没搜、搜了没成功）就想交结论：退回去要求先搜（真机上见过云端模型凭印象 1 秒就给了结论）；
// 退回后还是没拿到资料，按没查到资料收尾，不交出结论。同一步里既搜又交结论：先搜，这一步的结论不算。
func TestResearchMustSearchBeforeConcluding(t *testing.T) {
	searchDown := errors.New("搜索服务挂了")
	cases := []struct {
		name         string
		replies      []brain.ReplyDelta
		searchErr    error
		status       Status
		conclusion   string
		searches     int
		reasonHasErr bool // 原因里带上没搜成的说明
	}{
		{name: "直接写文字交差：退回后去搜", replies: []brain.ReplyDelta{
			{Text: "凭印象的结论。\n来源：https://example.com/x"}, call("search_web", map[string]any{"query": "真的去搜"})},
			status: StatusDone, conclusion: scriptedConclusion, searches: 1},
		{name: "直接用 submit_conclusion 交差：退回后去搜", replies: []brain.ReplyDelta{
			submit("凭印象的结论。"), call("search_web", map[string]any{"query": "真的去搜"})},
			status: StatusDone, conclusion: scriptedConclusion, searches: 1},
		{name: "退回后还是不搜：没查到资料，不交出结论", replies: []brain.ReplyDelta{
			submit("凭印象的结论。"), submit("还是凭印象的结论。")},
			status: StatusFailed, conclusion: "", searches: 0},
		{name: "搜了但没搜成：退回；还是没资料就收尾，原因带上没搜成的说明", replies: []brain.ReplyDelta{
			call("search_web", map[string]any{"query": "x"}), submit("凭印象的结论。"),
			call("search_web", map[string]any{"query": "再搜"}), submit("还是凭印象的结论。")},
			searchErr: searchDown, status: StatusFailed, conclusion: "", searches: 2, reasonHasErr: true},
		{name: "同一步里既搜又交结论：先搜，这一步的结论不算", replies: []brain.ReplyDelta{
			{ToolCalls: []brain.ToolCall{{Name: "search_web", Arguments: map[string]any{"query": "A"}},
				{Name: "submit_conclusion", Arguments: map[string]any{"conclusion": "没看结果就写的结论。"}}}},
			submit("看了结果写的结论。")},
			status: StatusDone, conclusion: "看了结果写的结论。", searches: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := &scriptedModel{replies: c.replies}
			rig := newRig(t, model, rigSetup{})
			rig.searcher.searchErr = c.searchErr
			done := rig.research(t, "要查的事")
			searches, _ := rig.searcher.counts()
			if done.Status != c.status || done.Conclusion != c.conclusion || searches != c.searches {
				t.Errorf("得到 %s「%s」、搜了 %d 次，应为 %s「%s」、%d 次（%+v）", done.Status, done.Conclusion, searches,
					c.status, c.conclusion, c.searches, done)
			}
			if c.reasonHasErr && !strings.Contains(done.StopReason, searchDown.Error()) {
				t.Errorf("原因里应带上没搜成的说明：%q", done.StopReason)
			}
		})
	}
}

// 搜索阶段模型出错：已经查到资料时不丢掉，和到上限一样用它们写结论；一份资料都没有才记失败。
func TestResearchModelErrorKeepsWhatWasFound(t *testing.T) {
	unavailable := errors.Join(brain.ErrModelUnavailable, errors.New("overloaded"))
	for _, c := range []struct {
		name       string
		model      *scriptedModel
		status     Status
		conclusion string
	}{
		{"搜到之后出错：用查到的写结论", &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A"})},
			failures: map[int]error{2: unavailable}}, StatusPartial, limitConclusion},
		{"什么都没查到就出错：记失败", &scriptedModel{failures: map[int]error{1: unavailable}}, StatusFailed, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			done := newRig(t, c.model, rigSetup{}).research(t, "要查的事")
			if done.Status != c.status || done.Conclusion != c.conclusion || done.StopReason == "" {
				t.Errorf("得到 %+v，应为 %s「%s」", done, c.status, c.conclusion)
			}
		})
	}
}

// 到上限时一份资料都没拿到（搜索一直没成功）：不让模型凭印象写结论，按没查到资料收尾。
func TestResearchAtLimitsWithoutMaterialWritesNothing(t *testing.T) {
	search := call("search_web", map[string]any{"query": "x"})
	model := &scriptedModel{repeat: &search}
	rig := newRig(t, model, rigSetup{rounds: 2})
	rig.searcher.searchErr = errors.New("搜索服务挂了")
	done := rig.research(t, "要查的事")
	if done.Status != StatusFailed || done.Conclusion != "" || !strings.Contains(done.StopReason, "搜索服务挂了") {
		t.Errorf("一份资料都没有：不写结论，原因带上没搜成的说明：%+v", done)
	}
	if searches, _ := rig.searcher.counts(); rig.budget.recorded() != searches || rig.budget.incompleteCharges() != searches {
		t.Errorf("发出去没搜成的请求也记账（标不完整）：搜了 %d 次，记了 %d 积分、%d 笔不完整", searches,
			rig.budget.recorded(), rig.budget.incompleteCharges())
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if want := []bool{true, true}; !slices.Equal(model.toolsSeen, want) {
		t.Errorf("到上限后不该再请模型写结论：%v", model.toolsSeen)
	}
}

// read_pages 一次给的网址超过上限：只读前面的，告诉模型哪些没读（需要的话再分批读）。
func TestReadPagesTellsWhichURLsWereNotRead(t *testing.T) {
	urls := []any{"https://example.com/1", "https://example.com/2", "https://example.com/3", "https://example.com/4", "https://example.com/5"}
	model := &scriptedModel{replies: []brain.ReplyDelta{call("read_pages", map[string]any{"urls": urls})}}
	rig := newRig(t, model, rigSetup{})
	if done := rig.research(t, "要查的事"); done.Status != StatusDone {
		t.Fatalf("读到的网页算资料，应能交结论：%+v", done)
	}
	rig.searcher.mu.Lock()
	read, chargedFor := rig.searcher.readURLs, rig.searcher.chargedFor
	rig.searcher.mu.Unlock()
	if want := []string{"https://example.com/1", "https://example.com/2", "https://example.com/3"}; !slices.Equal(read, want) || chargedFor != maxPagesPerRead {
		t.Errorf("只读前 %d 个、按 %d 页记账：读了 %v，按 %d 页记账", maxPagesPerRead, maxPagesPerRead, read, chargedFor)
	}
	var result readPagesOutput
	for _, message := range model.messagesOfLastRequest() {
		if message.Role == brain.RoleTool && message.ToolName == "read_pages" {
			if err := json.Unmarshal([]byte(message.Text), &result); err != nil {
				t.Fatalf("读网页交给模型的结果应是 JSON：%v %q", err, message.Text)
			}
		}
	}
	if want := []string{"https://example.com/4", "https://example.com/5"}; !slices.Equal(result.NotRead, want) || result.Note == "" ||
		len(result.Pages) != maxPagesPerRead {
		t.Errorf("交给模型的结果里应写明没读哪几个（读了的不算）：%+v", result)
	}
}

// gatedStore 让第一次建任务卡住，直到测试放行；每次数在查的任务时报一声。用来让两个派调查的请求恰好撞在一起。
type gatedStore struct {
	TaskStore
	activeCalls chan struct{}
	creating    chan struct{} // 第一次建任务开始时关上
	release     chan struct{} // 关上后第一次建任务才继续
	mu          sync.Mutex
	created     int
}

func (s *gatedStore) Active(ctx context.Context) ([]Task, error) {
	s.activeCalls <- struct{}{}
	return s.TaskStore.Active(ctx)
}

func (s *gatedStore) Create(ctx context.Context, person, question string) (Task, error) {
	s.mu.Lock()
	s.created++
	first := s.created == 1
	s.mu.Unlock()
	if first {
		close(s.creating)
		<-s.release
	}
	return s.TaskStore.Create(ctx, person, question)
}

// 并发派调查：数在查的任务和建任务连在一起做，不会两个都看到还没满、一起建出来超过同时进行的上限。
func TestStartRespectsPendingLimitUnderConcurrency(t *testing.T) {
	store := &gatedStore{TaskStore: openTestStore(t), activeCalls: make(chan struct{}, 4), creating: make(chan struct{}),
		release: make(chan struct{})}
	runner := newTestRunner(t, store, &scriptedModel{}, &fakeSearcher{}, &fakeBudget{}, rigSetup{})
	close(runner.ready) // 不跑任务：只看派任务
	ctx := context.Background()
	for i := 1; i < maxPending; i++ { // 先占满到只剩一个名额（直接建在账本里：不经过下面的闸）
		if _, err := store.TaskStore.Create(ctx, "主人", "已经在排队的 "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, 2)
	go func() { _, _, err := runner.Start(ctx, "主人", "第一个"); results <- err }()
	<-store.activeCalls
	<-store.creating // 第一个已经数过、正在建
	go func() { _, _, err := runner.Start(ctx, "别人", "第二个"); results <- err }()
	select {
	case <-store.activeCalls: // 第二个没等第一个建完就去数了（没加锁时会这样）
	case <-time.After(200 * time.Millisecond): // 第二个在等第一个建完
	}
	close(store.release)
	var succeeded int
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	active, err := store.TaskStore.Active(ctx)
	if err != nil || succeeded != 1 || len(active) != maxPending {
		t.Errorf("只剩一个名额：应只派出一个，派出 %d 个、在查或排队的有 %d 个（上限 %d，%v）", succeeded, len(active), maxPending, err)
	}
}

// 查完通知（让终端响一声）；主人自己取消的不通知。
func TestResearchNotifiesWhenFinished(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "x"})}, entered: make(chan struct{}, 64)}
	rig := newRig(t, model, rigSetup{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.run(ctx)
	task, _, _ := rig.runner.Start(ctx, "主人", "要查的事")
	if got := rig.waitFinished(t, task.ID); got.Person != "主人" || got.Status != StatusDone {
		t.Errorf("通知的任务不对：%+v", got)
	}

	for len(model.entered) > 0 { // 第一个已经查完：它的请求都结束了
		<-model.entered
	}
	model.setGate(make(chan struct{})) // 第二个卡在模型那里，直到被取消
	second, _, _ := rig.runner.Start(ctx, "主人", "会被取消的")
	waitEntered(t, model.entered)
	if _, err := rig.runner.Cancel(ctx, "主人", second.ID); err != nil {
		t.Fatal(err)
	}
	model.setGate(nil)
	model.mu.Lock()
	model.replies = []brain.ReplyDelta{call("search_web", map[string]any{"query": "y"})}
	model.mu.Unlock()
	third, _, _ := rig.runner.Start(ctx, "主人", "取消之后的")
	// 任务按顺序一个一个查：第二个要是也通知了，会排在第三个前面
	select {
	case got := <-rig.finished:
		if got.ID != third.ID {
			t.Errorf("取消的不该通知：%+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("第三个查完没有通知")
	}
}

// 派出后最长要等多久：搜索阶段的时限加写结论的时间；有审查时再加审查一次和复查的时限。
func TestLongestWaitFollowsLimits(t *testing.T) {
	for _, timeout := range []time.Duration{3 * time.Minute, 90 * time.Second} {
		limits := DefaultLimits
		limits.Timeout = timeout
		runner := newTestRunner(t, openTestStore(t), &scriptedModel{}, &fakeSearcher{}, &fakeBudget{}, rigSetup{limits: limits})
		if got := runner.LongestWait(); got != timeout+writeUpTimeout {
			t.Errorf("时限 %v + 写结论 %v：得到 %v", timeout, writeUpTimeout, got)
		}
		reviewed := newTestRunner(t, openTestStore(t), &scriptedModel{}, &fakeSearcher{}, &fakeBudget{},
			rigSetup{limits: limits, reviewer: &reviewerModel{}, reviewTimeout: time.Minute})
		if got, want := reviewed.LongestWait(), timeout+writeUpTimeout+time.Minute+recheckTimeout; got != want {
			t.Errorf("有审查时再加审查和复查的时限：得到 %v，应为 %v", got, want)
		}
	}
}

// 任务板一次读出这个人在查的、查完还没说的：查完说过的、取消的、别人的都不在里面。
func TestTaskStoreBoard(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	create := func(person, question string) int64 {
		task, err := store.Create(ctx, person, question)
		if err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	queued := create("主人", "排队中")
	running := create("主人", "在查")
	if marked, err := store.MarkRunning(ctx, running); err != nil || !marked {
		t.Fatalf("排队中的应能改成在查：%v %v", marked, err)
	}
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
	store := openTestStore(t)
	task, err := store.Create(ctx, "主人", "A 和 B 哪个便宜")
	if err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkRunning(ctx, task.ID); err != nil || !marked {
		t.Fatalf("排队中的应能改成在查：%v %v", marked, err)
	}
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

// 改成在查只对排队中的任务生效：已经在查、已经取消的改不到，交回没改到（runTask 据此不开查）。
func TestMarkRunningReportsWhetherItChanged(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	queued, err := store.Create(ctx, "主人", "排队中")
	if err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkRunning(ctx, queued.ID); err != nil || !marked {
		t.Errorf("排队中的应改成在查：%v %v", marked, err)
	}
	if marked, err := store.MarkRunning(ctx, queued.ID); err != nil || marked {
		t.Errorf("已经在查的不算改到：%v %v", marked, err)
	}
	cancelled, err := store.Create(ctx, "主人", "取消了")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(ctx, cancelled.ID, StatusCancelled, "", nil, "主人说不查了"); err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkRunning(ctx, cancelled.ID); err != nil || marked {
		t.Errorf("已经取消的改不到：%v %v", marked, err)
	}
}

// 来源存成 JSON 数组：来源里带换行（模型可以这样交）读回来还是一条，不会被切成两条。
func TestTaskSourcesKeepNewlines(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	task, err := store.Create(ctx, "主人", "A 和 B 哪个便宜")
	if err != nil {
		t.Fatal(err)
	}
	sources := []string{"https://example.com/a\n第二行", "https://example.com/b"}
	if _, err := store.Finish(ctx, task.ID, StatusDone, "A 便宜。", sources, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ctx, task.ID); err != nil || !slices.Equal(got.Sources, sources) {
		t.Errorf("来源应原样读回：%q %v", got.Sources, err)
	}
}

// faultyStore 是真的任务账本（SQLite），按测试的要求让接下来几次操作出错，或在第一次读到任务之后插一件事。
type faultyStore struct {
	TaskStore
	mu         sync.Mutex
	failGets   int        // 接下来这么多次 Get 出错
	failFinish int        // 接下来这么多次 Finish 出错
	failBoard  bool       // Board 一律出错
	afterGet   func(Task) // 第一次读到任务之后要做的事（只做一次）
}

var errStoreDown = errors.New("database is locked")

func (s *faultyStore) Get(ctx context.Context, id int64) (Task, error) {
	s.mu.Lock()
	if s.failGets > 0 {
		s.failGets--
		s.mu.Unlock()
		return Task{}, errStoreDown
	}
	after := s.afterGet
	s.afterGet = nil
	s.mu.Unlock()
	task, err := s.TaskStore.Get(ctx, id)
	if after != nil && err == nil {
		after(task)
	}
	return task, err
}

func (s *faultyStore) Finish(ctx context.Context, id int64, status Status, conclusion string, sources []string, reason string) (bool, error) {
	s.mu.Lock()
	if s.failFinish > 0 {
		s.failFinish--
		s.mu.Unlock()
		return false, errStoreDown
	}
	s.mu.Unlock()
	return s.TaskStore.Finish(ctx, id, status, conclusion, sources, reason)
}

func (s *faultyStore) Board(ctx context.Context, person string) ([]Task, error) {
	s.mu.Lock()
	fail := s.failBoard
	s.mu.Unlock()
	if fail {
		return nil, errStoreDown
	}
	return s.TaskStore.Board(ctx, person)
}

// newFaultyRig 同 newRig，任务账本换成按要求出错的那份。
func newFaultyRig(t *testing.T, model brain.ChatModel, store *faultyStore) *researchRig {
	t.Helper()
	store.TaskStore = openTestStore(t)
	rig := &researchRig{store: store, searcher: &fakeSearcher{}, budget: &fakeBudget{}, finished: make(chan Task, 16)}
	rig.runner = newTestRunner(t, store, model, rig.searcher, rig.budget, rigSetup{})
	rig.runner.OnFinished(func(task Task) { rig.finished <- task })
	return rig
}

// finishedUntil 收下结束的任务，直到 id 这个（含）；返回收到的全部。时限只是防止测试卡死。
func (rig *researchRig) finishedUntil(t *testing.T, id int64) []Task {
	t.Helper()
	var seen []Task
	deadline := time.After(5 * time.Second)
	for {
		select {
		case task := <-rig.finished:
			seen = append(seen, task)
			if task.ID == id {
				return seen
			}
		case <-deadline:
			t.Fatalf("任务 #%d 没结束（收到 %+v）", id, seen)
		}
	}
}

// 查完了，结论却没存上：不报「查完了」，改记成失败（任务板上告诉主人没存上），记上了才通知。
func TestResultNotSavedIsRecordedAsFailure(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A B 价格"})}}
	rig := newFaultyRig(t, model, &faultyStore{failFinish: 1})
	done := rig.research(t, "A 和 B 哪个便宜")
	if done.Status != StatusFailed || done.Conclusion != "" || !strings.Contains(done.StopReason, errStoreDown.Error()) {
		t.Errorf("结论没存上：应记成失败、写明原因，不报查完：%+v", done)
	}
}

// 结论没存上、改记失败也记不上：不通知（终端不响「查完了」），任务留在账本里等重启时标成被中断；后面的任务照常查。
func TestResultNotSavedAtAllIsNotNotified(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "第一个"}),
		submit(scriptedConclusion, "https://example.com/a"), call("search_web", map[string]any{"query": "第二个"})}}
	rig := newFaultyRig(t, model, &faultyStore{failFinish: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.run(ctx)
	first, _, err := rig.runner.Start(ctx, "主人", "第一个")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := rig.runner.Start(ctx, "主人", "第二个")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range rig.finishedUntil(t, second.ID) {
		if task.ID == first.ID {
			t.Errorf("结果没存上的不该通知：%+v", task)
		}
	}
	if task, err := rig.store.Get(ctx, first.ID); err != nil || task.Status != StatusRunning {
		t.Errorf("没存上的留在账本里（在查，重启时标成被中断）：%+v %v", task, err)
	}
}

// 读不出排队的任务：记成失败并通知（任务板上告诉主人），不让它一直「排队中」占着名额；也不开查。
func TestQueuedTaskThatCannotBeReadIsRecordedAsFailure(t *testing.T) {
	model := &scriptedModel{}
	rig := newFaultyRig(t, model, &faultyStore{failGets: 1})
	done := rig.research(t, "A 和 B 哪个便宜")
	if done.Status != StatusFailed || !strings.Contains(done.StopReason, errStoreDown.Error()) {
		t.Errorf("读不出来的任务应记成失败、写明原因：%+v", done)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if model.requests != 0 {
		t.Errorf("读不出来就不开查：请求了模型 %d 次", model.requests)
	}
}

// 读到任务之后、开查之前刚好被取消（这时还没登记取消用的函数，Cancel 只改了账本）：改不成在查，就不开查、不通知。
func TestTaskCancelledBeforeMarkRunningIsNotInvestigated(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "第二个"})}}
	store := &faultyStore{}
	rig := newFaultyRig(t, model, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.afterGet = func(task Task) {
		if _, err := rig.runner.Cancel(ctx, "主人", task.ID); err != nil {
			t.Errorf("取消：%v", err)
		}
	}
	rig.run(ctx)
	first, _, err := rig.runner.Start(ctx, "主人", "第一个")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := rig.runner.Start(ctx, "主人", "第二个")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range rig.finishedUntil(t, second.ID) {
		if task.ID == first.ID {
			t.Errorf("取消了的不该通知：%+v", task)
		}
	}
	model.mu.Lock()
	questions := slices.Clone(model.questions)
	model.mu.Unlock()
	if slices.Contains(questions, "第一个") {
		t.Errorf("开查之前被取消的不该查：模型收到的问题 %v", questions)
	}
	if task, err := rig.store.Get(ctx, first.ID); err != nil || task.Status != StatusCancelled {
		t.Errorf("应保持取消：%+v %v", task, err)
	}
}

// 任务板读不出来：记警告，并告诉模型这一轮不知道调查的情况（不当成没有调查）。
func TestBriefSaysWhenBoardCannotBeRead(t *testing.T) {
	rig := newFaultyRig(t, &scriptedModel{}, &faultyStore{failBoard: true})
	if board := rig.runner.Brief("主人"); !slices.Equal(board.Lines, []string{boardUnreadableNote}) || board.Delivered != nil {
		t.Errorf("任务板读不出来：应告诉模型不知道调查的情况：%+v", board)
	}
}
