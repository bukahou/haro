package research

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/tools"
)

// Limits 是每个调查任务的硬上限（R2）：由程序强制，不靠模型自己停。先到先停。
type Limits struct {
	Timeout     time.Duration // 搜索阶段最多多久（默认 3 分钟）
	MaxRounds   int           // 最多请求模型几轮（默认 10）
	MaxSearches int           // 最多搜几次（默认 5）
	MaxTokens   int           // 累计 token 上限（默认 8 万）
	PageRunes   int           // 每个网页只读前多少字（默认 2,000）
	MaxPending  int           // 全家最多同时有几个任务（1 个在查 + 排队；默认 3）
}

// DefaultLimits 是 R2 裁定的默认值。
var DefaultLimits = Limits{Timeout: 3 * time.Minute, MaxRounds: 10, MaxSearches: 5, MaxTokens: 80_000, PageRunes: 2000, MaxPending: 3}

const (
	writeUpTimeout  = 45 * time.Second // 到上限后写结论的时间（不算在搜索阶段里）
	searchResults   = 5                // 调查时每次搜索的条数
	maxPagesPerRead = 3                // 一次最多读几页
)

// Budget 是调查的额度（spending.Guard 实现）：派任务前、每一轮之前都查一次。
type Budget interface {
	AllowResearch(ctx context.Context) error
}

// Runner 排队执行调查任务：全家同时只跑 1 个（R2），其余排队。
type Runner struct {
	store    TaskStore
	model    brain.ChatModel // 调查用的模型（R6：单独配置，默认云端），已按 research 用途记账
	writer   brain.ChatModel // 写结论的模型（nil = 调查模型自己写）；见 WriteConclusionsWith
	searcher tools.WebSearcher
	meter    tools.SearchMeter // 按 research 用途查额度、记账
	budget   Budget
	limits   Limits
	logger   *slog.Logger

	queue chan int64
	ready chan struct{} // Run 做完启动清理（把上次没查完的标成被中断）后关闭；Start 等它，免得新任务被误标

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

// NewRunner 创建调查执行器；要再调用 Run 才开始干活。
func NewRunner(store TaskStore, model brain.ChatModel, searcher tools.WebSearcher, meter tools.SearchMeter, budget Budget,
	limits Limits, logger *slog.Logger) *Runner {
	return &Runner{store: store, model: model, searcher: searcher, meter: meter, budget: budget, limits: limits,
		logger: logger.With("component", "research"), queue: make(chan int64, 64), ready: make(chan struct{}),
		cancels: map[int64]context.CancelFunc{}}
}

// WriteConclusionsWith 让另一个模型写结论：调查模型（便宜的）负责搜索和读网页，搜完把全部资料交给 writer（更强的）
// 写结论，不给工具。writer 出错时：查够了的用调查模型自己的草稿，到上限的由调查模型写。要在 Run 之前调用。
func (r *Runner) WriteConclusionsWith(writer brain.ChatModel) {
	r.writer = writer
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
	active, err := r.store.Active(ctx)
	if err != nil {
		return Task{}, 0, err
	}
	if len(active) >= r.limits.MaxPending {
		return Task{}, 0, fmt.Errorf("现在已经有 %d 个调查在查或排队，等前面的查完再派", len(active))
	}
	task, err := r.store.Create(ctx, person, question)
	if err != nil {
		return Task{}, 0, err
	}
	select {
	case r.queue <- task.ID:
	default:
		_, _ = r.store.Finish(ctx, task.ID, StatusFailed, "", nil, "队列满了")
		return Task{}, 0, errors.New("调查队列满了")
	}
	r.logger.Info("派出调查", "id", task.ID, "person", person, "question", question, "ahead", len(active))
	return task, len(active), nil
}

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
	if err != nil || task.Status != StatusQueued { // 排队时被取消了
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
	if err := r.store.MarkRunning(ctx, id); err != nil {
		r.logger.Warn("标记在查失败", "id", id, "err", err)
	}
	started := time.Now()
	outcome := r.investigate(taskCtx, task.Question)
	if taskCtx.Err() != nil && ctx.Err() == nil { // 主人取消了：Cancel 已经记过
		r.logger.Info("调查已取消", "id", id, "elapsed", time.Since(started).Round(time.Second))
		return
	}
	if ctx.Err() != nil { // 服务端在关：下次启动时标成被中断
		return
	}
	finished, err := r.store.Finish(context.WithoutCancel(ctx), id, outcome.status, outcome.conclusion, outcome.sources, outcome.reason)
	switch {
	case err != nil:
		r.logger.Warn("调查结果没存上", "id", id, "err", err)
	case !finished: // 查完的同时主人取消了：以取消为准，不报查完
		r.logger.Info("调查已取消（结果作废）", "id", id)
		return
	}
	r.logger.Info("调查结束", "id", id, "status", outcome.status, "reason", outcome.reason, "rounds", outcome.rounds,
		"searches", outcome.searches, "tokens", outcome.tokens, "elapsed", time.Since(started).Round(time.Second))
	r.mu.Lock()
	callback := r.finished
	r.mu.Unlock()
	if callback != nil {
		if finished, err := r.store.Get(context.WithoutCancel(ctx), id); err == nil {
			callback(finished)
		}
	}
}

// outcome 是一次调查的结果。
type outcome struct {
	status          Status
	conclusion      string
	sources         []string
	reason          string
	rounds          int
	searches, reads int
	tokens          int
}

// investigate 是调查的主循环：模型决定搜什么、读哪几页；到任何一个上限就不再给工具，逼它用已有材料写结论。
func (r *Runner) investigate(ctx context.Context, question string) outcome {
	searchCtx, cancel := context.WithTimeout(ctx, r.limits.Timeout)
	defer cancel()
	messages := []brain.ChatMessage{
		{Role: brain.RoleSystem, Text: researchPrompt(r.limits, time.Now())},
		{Role: brain.RoleUser, Text: question},
	}
	var result outcome
	stop := ""
	nudged := false // 已经因为「没搜就交差」退回过一次
	for {
		switch {
		case result.rounds >= r.limits.MaxRounds:
			stop = fmt.Sprintf("已经查了 %d 轮（上限）", result.rounds)
		case result.tokens >= r.limits.MaxTokens:
			stop = fmt.Sprintf("用了约 %d token（上限 %d）", result.tokens, r.limits.MaxTokens)
		case searchCtx.Err() != nil:
			stop = fmt.Sprintf("查了 %s（时间上限）", r.limits.Timeout)
		}
		if stop == "" {
			if err := r.budget.AllowResearch(ctx); err != nil {
				stop = err.Error()
			}
		}
		if stop != "" {
			break
		}
		result.rounds++
		text, calls, tokens, err := complete(searchCtx, r.model, messages, researchToolSpecs)
		result.tokens += tokens
		if err != nil {
			if ctx.Err() != nil {
				return result
			}
			if searchCtx.Err() != nil {
				continue // 下一圈判成时间上限
			}
			result.status, result.reason = StatusFailed, "模型出错："+err.Error()
			return result
		}
		draft, submitted := submittedConclusion(calls, r.logger)
		if !submitted && len(calls) == 0 && result.searches == 0 && !nudged {
			// 一次都没搜就想交差（2026-11-17 真机：云端模型凭印象 1 秒给了结论）：退回去，要求先搜
			nudged = true
			messages = append(messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text},
				brain.ChatMessage{Role: brain.RoleUser, Text: "你还没有搜索。这是调查任务，结论必须以搜到的资料为准：先用 search_web 搜，再下结论。"})
			continue
		}
		if submitted || len(calls) == 0 { // 模型认为查够了，交出了结论
			result.status = StatusDone
			if !submitted { // 没用 submit_conclusion、直接写了文字：整段当结论（没有单独的来源）
				draft = conclusionDraft{Conclusion: strings.TrimSpace(text)}
				r.logger.Warn("调查模型没用 submit_conclusion 交结论：整段文字当结论")
			}
			if r.writer != nil { // 交给写结论的模型重写（不带调查模型的草稿，免得被它带偏）；写不成就用草稿
				instruction := brain.ChatMessage{Role: brain.RoleUser, Text: "资料已经查够了。不要再搜索，根据上面查到的资料写结论，用 submit_conclusion 交出来。"}
				if written, err := r.writeConclusion(ctx, append(messages, instruction), &result, r.writer); err == nil {
					draft = written
				} else if ctx.Err() == nil {
					r.logger.Warn("写结论的模型出错，用调查模型的草稿", "err", err)
				}
			}
			result.conclusion, result.sources = draft.Conclusion, draft.Sources
			if result.conclusion == "" {
				result.status, result.reason = StatusFailed, "模型没有给出结论"
			}
			return result
		}
		for i := range calls {
			if calls[i].ID == "" {
				calls[i].ID = fmt.Sprintf("r%d-%d", result.rounds, i)
			}
		}
		messages = append(messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text, ToolCalls: calls})
		for _, call := range calls {
			messages = append(messages, brain.ChatMessage{Role: brain.RoleTool, ToolName: call.Name, ToolCallID: call.ID,
				Text: r.runTool(searchCtx, call, question, &result)})
		}
	}

	// 到上限：不再给工具，用已有材料写结论（写结论的时间单算；超出的这一点费用在额度上允许）
	messages = append(messages, brain.ChatMessage{Role: brain.RoleUser,
		Text: "调查到此为止（" + stop + "）。不要再搜索，根据上面已经查到的资料写结论，用 submit_conclusion 交出来；没查清的地方明确说没查完。"})
	writers := []brain.ChatModel{r.model}
	if r.writer != nil {
		writers = []brain.ChatModel{r.writer, r.model} // 写结论的模型出错时由调查模型写
	}
	draft, err := r.writeConclusion(ctx, messages, &result, writers...)
	result.reason = stop
	if err != nil {
		if ctx.Err() != nil {
			return result
		}
		result.status = StatusFailed
		result.reason = stop + "；写结论时出错：" + err.Error()
		return result
	}
	result.status = StatusPartial
	result.conclusion, result.sources = draft.Conclusion, draft.Sources
	return result
}

// writeConclusion 依次让 writers 根据 messages 里的资料写结论（只给 submit_conclusion），第一个写成的为准；
// 每个模型各有 writeUpTimeout。没用工具、直接写了文字的，整段当结论。
func (r *Runner) writeConclusion(ctx context.Context, messages []brain.ChatMessage, result *outcome, writers ...brain.ChatModel) (conclusionDraft, error) {
	var lastErr error
	for i, writer := range writers {
		writeCtx, cancel := context.WithTimeout(ctx, writeUpTimeout)
		text, calls, tokens, err := complete(writeCtx, writer, messages, []brain.ToolSpec{submitConclusionSpec})
		cancel()
		result.tokens += tokens
		if err == nil {
			if draft, ok := submittedConclusion(calls, r.logger); ok && draft.Conclusion != "" {
				return draft, nil
			}
			if text = strings.TrimSpace(text); text != "" {
				r.logger.Warn("写结论的模型没用 submit_conclusion：整段文字当结论")
				return conclusionDraft{Conclusion: text}, nil
			}
			err = errors.New("没写出结论")
		}
		if ctx.Err() != nil {
			return conclusionDraft{}, ctx.Err()
		}
		lastErr = err
		if i < len(writers)-1 {
			r.logger.Warn("写结论的模型出错，换下一个写", "err", err)
		}
	}
	return conclusionDraft{}, lastErr
}

// conclusionDraft 是模型用 submit_conclusion 交出的结论：结论会被念出来，来源只存档。
type conclusionDraft struct {
	Conclusion string   `json:"conclusion" jsonschema:"3 到 6 句中文口语的结论（会被念出来：不要列表、表格、网址、Markdown）；有不确定或没查到的地方直接说"`
	Sources    []string `json:"sources,omitempty" jsonschema:"1 到 3 个最主要的来源网址"`
}

// submittedConclusion 找出这一轮里的 submit_conclusion 调用并解出参数；没有、或者参数不对时 ok = false。
func submittedConclusion(calls []brain.ToolCall, logger *slog.Logger) (conclusionDraft, bool) {
	for _, call := range calls {
		if call.Name != submitConclusionSpec.Name {
			continue
		}
		draft, err := brain.DecodeArguments[conclusionDraft](call.Arguments)
		if err != nil {
			logger.Warn("submit_conclusion 的参数不对", "err", err)
			return conclusionDraft{}, false
		}
		draft.Conclusion = strings.TrimSpace(draft.Conclusion)
		return draft, true
	}
	return conclusionDraft{}, false
}

// runTool 执行调查里的一次工具调用，返回给模型看的文字（出错也作为文字交给模型，让它换个办法）。
func (r *Runner) runTool(ctx context.Context, call brain.ToolCall, question string, result *outcome) string {
	switch call.Name {
	case "search_web":
		if result.searches >= r.limits.MaxSearches {
			return fmt.Sprintf("搜索次数已经用完（上限 %d 次）。请根据已有资料写结论。", r.limits.MaxSearches)
		}
		in, err := brain.DecodeArguments[searchInput](call.Arguments)
		if err != nil || strings.TrimSpace(in.Query) == "" {
			return orError("", errors.Join(errors.New("query 不能为空"), err))
		}
		text, err := r.metered(ctx, r.searcher.SearchCredits(), func() (string, error) {
			return r.searcher.Search(ctx, in.Query, searchResults)
		})
		result.searches++
		r.logger.Info("调查：搜索", "query", in.Query, "err", err)
		return orError(text, err)
	case "read_pages":
		in, err := brain.DecodeArguments[readPagesInput](call.Arguments)
		if err != nil || len(in.URLs) == 0 {
			return orError("", errors.Join(errors.New("urls 不能为空"), err))
		}
		urls := in.URLs
		if len(urls) > maxPagesPerRead {
			urls = urls[:maxPagesPerRead]
		}
		text, err := r.metered(ctx, r.searcher.ReadCredits(len(urls)), func() (string, error) {
			return r.searcher.ReadPages(ctx, urls, r.limits.PageRunes, question)
		})
		result.reads++
		r.logger.Info("调查：读网页", "urls", len(urls), "err", err)
		return orError(text, err)
	default:
		return "没有叫 " + call.Name + " 的工具"
	}
}

// metered 先查额度、执行、成功后记账。
func (r *Runner) metered(ctx context.Context, credits int, run func() (string, error)) (string, error) {
	if err := r.meter.AllowSearch(ctx, credits); err != nil {
		return "", err
	}
	text, err := run()
	if err != nil {
		return "", err
	}
	if err := r.meter.RecordSearch(context.WithoutCancel(ctx), credits); err != nil {
		r.logger.Warn("调查的搜索用量没记上", "err", err)
	}
	return text, nil
}

func orError(text string, err error) string {
	if err != nil {
		return "失败：" + err.Error()
	}
	return text
}

// complete 请求模型一次，收齐文字、工具调用与 token 数（出错时也算上知道的用量）；被截断按失败（brain.ErrTruncated）。
func complete(ctx context.Context, model brain.ChatModel, messages []brain.ChatMessage, specs []brain.ToolSpec) (string, []brain.ToolCall, int, error) {
	reply, err := brain.CollectReply(ctx, model, brain.ChatRequest{Messages: messages, Tools: specs})
	return reply.Text, reply.ToolCalls, reply.Usage.PromptTokens + reply.Usage.OutputTokens, err
}

type searchInput struct {
	Query string `json:"query" jsonschema:"搜索词，简洁具体"`
}

type readPagesInput struct {
	URLs []string `json:"urls" jsonschema:"要读的网址，取自搜索结果"`
}

var submitConclusionSpec = brain.ToolSpec{Name: "submit_conclusion",
	Description: "交出调查结论（结论和来源分开）。查够了就调用它，不要把结论直接写在回答里。",
	Parameters:  brain.ParametersFor[conclusionDraft]()}

var researchToolSpecs = []brain.ToolSpec{
	{Name: "search_web", Description: "联网搜索，返回几条结果（标题、网址、摘要）。", Parameters: brain.ParametersFor[searchInput]()},
	{Name: "read_pages", Description: "读网页正文（一次最多 3 个网址，每页只给开头一部分）。摘要不够用时才读。",
		Parameters: brain.ParametersFor[readPagesInput]()},
	submitConclusionSpec,
}

func researchPrompt(limits Limits, now time.Time) string {
	// 告诉模型今天的日期（2026-11-17 实测：不说的话搜索词里写的是两年前的年份，搜回来的都是旧资料）
	today := fmt.Sprintf("今天是 %s（%s）。要查「现在」「最近」的事时按这个日期来，不要用别的年份。\n", now.Format("2006年1月2日"), brain.WeekdayName(now))
	return today + fmt.Sprintf(`你是调查助手，在后台替主人查资料。用 search_web 搜索、必要时用 read_pages 读正文，查清楚后直接给出结论。
限制：最多搜索 %d 次、最多 %d 轮；够用就停，不要为了凑数多查。搜索词按资料最可能使用的语言写（日本的事用日语搜，其他按情况用英语或中文）。
查清楚后调用 submit_conclusion 交出结论和来源。`, limits.MaxSearches, limits.MaxRounds)
}
