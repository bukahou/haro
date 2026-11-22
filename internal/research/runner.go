package research

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// 调查执行器的队列与生命周期：派任务、排队、一个一个查、取消、查完通知。查的过程见 investigate.go（查资料的工具 lookup.go，
// 审查与复查 review.go），任务板见 board.go。

// Limits 是每个调查任务的硬上限（R2）：由程序强制，不靠模型自己停。先到先停。配置可以改（HARO_RESEARCH_*）。
// 轮数也是上限之一，是调查的代理的步数（Agents.Investigator.MaxSteps：默认在代理定义 research.md 里）。
type Limits struct {
	Timeout     time.Duration // 搜索阶段最多多久（默认 3 分钟）
	MaxSearches int           // 最多搜几次（默认 5；没搜成的也算一次）
	MaxTokens   int           // 累计 token 上限（默认 8 万）
}

// DefaultLimits 是 R2 裁定的默认值（服务端没设 HARO_RESEARCH_TIMEOUT、HARO_RESEARCH_MAX_SEARCHES、HARO_RESEARCH_MAX_TOKENS 时
// 用它，默认值只写在这里）。
var DefaultLimits = Limits{Timeout: 3 * time.Minute, MaxSearches: 5, MaxTokens: 80_000}

// check 检查上限：都要大于 0（配置里没设的，装配处已经换成默认值）。
func (l Limits) check() error {
	if l.Timeout <= 0 || l.MaxSearches <= 0 || l.MaxTokens <= 0 {
		return fmt.Errorf("调查的上限都要大于 0：时限 %v、搜索 %d 次、%d token", l.Timeout, l.MaxSearches, l.MaxTokens)
	}
	return nil
}

const (
	// maxPending 是全家最多同时有几个调查（在查的加排队的）。不开放成配置：同时只查一个（R2），排队的要等前面的查完，
	// 每个最长 LongestWait，排多了主人要等很久；R2 定的是 1 个在查、2 个排队。
	maxPending = 3
	// queueCapacity 是排队的队列能放几个。排队时被取消的任务还占着队列里的位置（Run 取到时才跳过），所以要比 maxPending
	// 大得多；满了（一个任务查着的时候连着派了又取消几十次）时先不派，不建任务（见 Start）。
	queueCapacity = 64
)

// Runner 排队执行调查任务：全家同时只跑 1 个（R2），其余排队。
type Runner struct {
	store    TaskStore
	agents   Agents // 调查、审查的代理（模型已按 research 用途记账）
	searcher Searcher
	meter    Meter // 按 research 用途查额度、记账
	budget   Budget
	limits   Limits
	logger   *slog.Logger
	now      func() time.Time // 告诉模型的现在的日期按它（测试可以换）

	// 交给模型的工具（按两个代理的允许名单筛好）：调查时的、到上限写结论时的（只有交结论）、审查的
	investigationSpecs, submitSpecs, reviewSpecs []brain.ToolSpec

	queue chan int64
	ready chan struct{} // Run 做完启动清理（把上次没查完的标成被中断）后关闭；Start 等它，免得新任务被误标

	// creating 让「数在查的任务」和「建新任务」连在一起做：并发派调查时不会都看到还没满、一起建出来超过 maxPending；
	// 往队列里放也只在它锁着时做（见 Start）
	creating sync.Mutex

	mu       sync.Mutex
	cancels  map[int64]context.CancelFunc // 在查的任务 → 取消它
	finished func(Task)                   // 任务结束（查完 / 没查完 / 失败）时调用；主人取消的不调用
}

// OnFinished 设置任务结束时的回调（例如让主人最近说话的那台终端响一声）；随时可以设置。
func (r *Runner) OnFinished(callback func(Task)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = callback
}

// NewRunner 创建调查执行器；要再调用 Run 才开始干活。代理（见 Agents.check）、上限不对时报错。
func NewRunner(store TaskStore, agents Agents, searcher Searcher, meter Meter, budget Budget, limits Limits,
	logger *slog.Logger) (*Runner, error) {
	if err := errors.Join(agents.check(), limits.check()); err != nil {
		return nil, err
	}
	r := &Runner{store: store, agents: agents, searcher: searcher, meter: meter, budget: budget, limits: limits,
		logger: logger.With("component", "research"), now: time.Now,
		investigationSpecs: offered(agents.Investigator, searchWebSpec, readPagesSpec, submitConclusionSpec),
		submitSpecs:        offered(agents.Investigator, submitConclusionSpec),
		queue:              make(chan int64, queueCapacity), ready: make(chan struct{}), cancels: map[int64]context.CancelFunc{}}
	if agents.Reviewer != nil {
		r.reviewSpecs = offered(*agents.Reviewer, submitReviewSpec)
	}
	return r, nil
}

// LongestWait 是派出后最长要等多久：搜索阶段的时限加上到上限后写结论的时间；有审查时再加审查一次和复查的时限
// （到时限就用已经查到的资料收尾）。
func (r *Runner) LongestWait() time.Duration {
	longest := r.limits.Timeout + writeUpTimeout
	if r.agents.Reviewer != nil {
		longest += r.agents.Reviewer.Timeout + recheckTimeout
	}
	return longest
}

// Run 处理队列，直到 ctx 取消。启动时把上次没查完的标成「被中断」（下次对话时告诉主人）。
func (r *Runner) Run(ctx context.Context) {
	if n, err := r.store.InterruptActive(ctx); err != nil {
		r.logger.Warn("标记上次没查完的调查失败", "err", err)
	} else if n > 0 {
		r.logger.Info("上次服务端停止时没查完的调查已标为被中断", "count", n)
	}
	close(r.ready)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.queue:
			r.runTask(ctx, id)
		}
	}
}

// Start 派一个调查任务（主对话的 start_research 调用）；立刻返回，任务在后台排队执行。
func (r *Runner) Start(ctx context.Context, person, question string) (Task, int, error) {
	select {
	case <-r.ready:
	case <-ctx.Done():
		return Task{}, 0, ctx.Err()
	}
	if err := r.budget.AllowResearch(ctx); err != nil {
		return Task{}, 0, err
	}
	r.creating.Lock()
	defer r.creating.Unlock()
	active, err := r.store.Active(ctx)
	if err != nil {
		return Task{}, 0, err
	}
	if len(active) >= maxPending {
		return Task{}, 0, fmt.Errorf("现在已经有 %d 个调查在查或排队，等前面的查完再派", len(active))
	}
	if len(r.queue) == cap(r.queue) { // 先看有没有位置再建任务：放不进队列的任务不建（建了再记失败，主人会听到两次）
		r.logger.Warn("调查队列满了（排队时取消的还占着位置）：这次不派", "capacity", cap(r.queue))
		return Task{}, 0, errQueueFull
	}
	task, err := r.store.Create(ctx, person, question)
	if err != nil {
		return Task{}, 0, err
	}
	r.queue <- task.ID // 只有这里往队列里放（creating 锁着），上面看过有位置：不会等
	r.logger.Info("派出调查", "id", task.ID, "person", person, "question", question, "ahead", len(active))
	return task, len(active), nil
}

// errQueueFull 是排队的队列满了、这次没派（没建任务）。
var errQueueFull = errors.New("调查队列满了（刚才取消的调查还没清掉），过一会儿再派")

// Cancel 取消某人的一个调查；id 为 0 时取消他最近一个没结束的。
func (r *Runner) Cancel(ctx context.Context, person string, id int64) (Task, error) {
	if id == 0 {
		recent, err := r.store.Recent(ctx, person, 10)
		if err != nil {
			return Task{}, err
		}
		for _, task := range recent {
			if !task.Status.Finished() {
				id = task.ID
				break
			}
		}
		if id == 0 {
			return Task{}, errors.New("现在没有在查的调查")
		}
	}
	task, err := r.store.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if task.Person != person {
		return Task{}, ErrTaskNotFound // 不能取消别人的
	}
	if task.Status.Finished() {
		return task, fmt.Errorf("调查 #%d 已经%s，不用取消", id, task.Status.Label())
	}
	r.mu.Lock()
	cancel := r.cancels[id]
	r.mu.Unlock()
	if cancel != nil {
		cancel() // 在查：让它停下（runTask 会记成取消）
	}
	cancelled, err := r.store.Finish(ctx, id, StatusCancelled, "", nil, "主人说不查了")
	if err != nil {
		return Task{}, err
	}
	if !cancelled { // 刚好在这之前查完了：以查完的为准
		task, err := r.store.Get(ctx, id)
		if err != nil {
			return Task{}, err
		}
		return task, fmt.Errorf("调查 #%d 刚好已经%s，没取消", id, task.Status.Label())
	}
	_ = r.store.MarkReported(ctx, id)
	r.logger.Info("取消调查", "id", id)
	return r.store.Get(ctx, id)
}

func (r *Runner) runTask(ctx context.Context, id int64) {
	task, err := r.store.Get(ctx, id)
	switch {
	case errors.Is(err, ErrTaskNotFound):
		r.logger.Warn("排队的调查在任务账本里找不到了：跳过", "id", id)
		return
	case err != nil:
		if ctx.Err() != nil { // 服务端在关：下次启动时标成被中断
			return
		}
		// 读不出来就查不了：记成失败（任务板上告诉主人），不让它一直「排队中」、占着名额
		r.logger.Error("读不出排队的调查：记成失败", "id", id, "err", err)
		r.finishFailed(ctx, id, "读不出这个调查任务："+err.Error())
		return
	case task.Status != StatusQueued: // 排队时被取消了（Cancel 已经记过）
		r.logger.Info("排队时已经结束的调查：跳过", "id", id, "status", task.Status)
		return
	}
	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.mu.Lock()
	r.cancels[id] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.cancels, id)
		r.mu.Unlock()
	}()
	running, err := r.store.MarkRunning(ctx, id)
	switch {
	case err != nil: // 状态没改成在查（任务板会说还在排队），照样开查：Finish 对排队中的也认
		r.logger.Warn("标记在查失败：照样开查", "id", id, "err", err)
	case !running: // 读到任务之后、登记取消之前被取消了（Cancel 已经记过）：不开查，不白花调查的额度
		r.logger.Info("开查之前被取消了", "id", id)
		return
	}
	started := time.Now()
	report := r.investigate(taskCtx, task.Question)
	if taskCtx.Err() != nil && ctx.Err() == nil { // 主人取消了：Cancel 已经记过
		r.logger.Info("调查已取消", "id", id, "elapsed", time.Since(started).Round(time.Second))
		return
	}
	if ctx.Err() != nil { // 服务端在关：下次启动时标成被中断
		return
	}
	status := report.status
	finished, err := r.store.Finish(context.WithoutCancel(ctx), id, status, report.conclusion, report.sources, report.reason)
	if err != nil {
		// 结论没存上就不报「查完了」（任务板上没有它）：改记成失败（只写原因，写得进的可能大些），记上了才通知，
		// 主人能从任务板听到；也记不上时不通知（重启后标成被中断）
		r.logger.Error("调查结果没存上：改记成失败", "id", id, "status", status, "err", err)
		status = StatusFailed
		if finished, err = r.store.Finish(context.WithoutCancel(ctx), id, status, "", nil, "调查结果没存上："+err.Error()); err != nil {
			r.logger.Error("调查结果没存上，也没能记成失败：不通知", "id", id, "err", err)
			return
		}
	}
	if !finished { // 查完的同时主人取消了：以取消为准，不报查完
		r.logger.Info("调查已取消（结果作废）", "id", id)
		return
	}
	r.logger.Info("调查结束", "id", id, "status", status, "reason", report.reason, "rounds", report.rounds,
		"searches", report.searches, "reads", report.reads, "gathered", report.gathered, "tokens", report.tokens,
		"review", report.review, "doubts", report.doubts, "recheck_rounds", report.recheckRounds, "review_tokens", report.reviewTokens,
		"elapsed", time.Since(started).Round(time.Second))
	r.notifyFinished(ctx, id)
}

// finishFailed 把还没开查的任务记成失败；记上了就通知（任务板上告诉主人）。
func (r *Runner) finishFailed(ctx context.Context, id int64, reason string) {
	finished, err := r.store.Finish(context.WithoutCancel(ctx), id, StatusFailed, "", nil, reason)
	switch {
	case err != nil:
		r.logger.Error("调查也没能记成失败：不通知", "id", id, "err", err)
	case finished:
		r.notifyFinished(ctx, id)
	}
}

// notifyFinished 把结束的任务（按账本里存下的样子）交给 OnFinished 的回调。
func (r *Runner) notifyFinished(ctx context.Context, id int64) {
	r.mu.Lock()
	callback := r.finished
	r.mu.Unlock()
	if callback == nil {
		return
	}
	task, err := r.store.Get(context.WithoutCancel(ctx), id)
	if err != nil {
		r.logger.Warn("调查结束了，但读不出这个任务：没通知终端", "id", id, "err", err)
		return
	}
	callback(task)
}
