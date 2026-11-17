package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

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
		_ = r.store.Finish(ctx, task.ID, StatusFailed, "", nil, "队列满了")
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
	if err := r.store.Finish(ctx, id, StatusCancelled, "", nil, "主人说不查了"); err != nil {
		return Task{}, err
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
	if err := r.store.Finish(context.WithoutCancel(ctx), id, outcome.status, outcome.conclusion, outcome.sources, outcome.reason); err != nil {
		r.logger.Warn("调查结果没存上", "id", id, "err", err)
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
		if len(calls) == 0 && result.searches == 0 && !nudged {
			// 一次都没搜就想交差（2026-11-17 真机：云端模型凭印象 1 秒给了结论）：退回去，要求先搜
			nudged = true
			messages = append(messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text},
				brain.ChatMessage{Role: brain.RoleUser, Text: "你还没有搜索。这是调查任务，结论必须以搜到的资料为准：先用 search_web 搜，再下结论。"})
			continue
		}
		if len(calls) == 0 { // 模型认为查够了，直接给出结论
			result.status = StatusDone
			result.conclusion, result.sources = splitSources(text)
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
	writeCtx, cancelWrite := context.WithTimeout(ctx, writeUpTimeout)
	defer cancelWrite()
	messages = append(messages, brain.ChatMessage{Role: brain.RoleUser,
		Text: "调查到此为止（" + stop + "）。不要再搜索，根据上面已经查到的资料写结论；没查清的地方明确说没查完。"})
	text, _, tokens, err := complete(writeCtx, r.model, messages, nil)
	result.tokens += tokens
	result.reason = stop
	if err != nil || strings.TrimSpace(text) == "" {
		if ctx.Err() != nil {
			return result
		}
		result.status = StatusFailed
		if err != nil {
			result.reason = stop + "；写结论时出错：" + err.Error()
		}
		return result
	}
	result.status = StatusPartial
	result.conclusion, result.sources = splitSources(text)
	return result
}

// runTool 执行调查里的一次工具调用，返回给模型看的文字（出错也作为文字交给模型，让它换个办法）。
func (r *Runner) runTool(ctx context.Context, call brain.ToolCall, question string, result *outcome) string {
	switch call.Name {
	case "search_web":
		if result.searches >= r.limits.MaxSearches {
			return fmt.Sprintf("搜索次数已经用完（上限 %d 次）。请根据已有资料写结论。", r.limits.MaxSearches)
		}
		query, _ := call.Arguments["query"].(string)
		if strings.TrimSpace(query) == "" {
			return "query 不能为空"
		}
		text, err := r.metered(ctx, r.searcher.SearchCredits(), func() (string, error) {
			return r.searcher.Search(ctx, query, searchResults)
		})
		result.searches++
		r.logger.Info("调查：搜索", "query", query, "err", err)
		return orError(text, err)
	case "read_pages":
		urls := stringList(call.Arguments["urls"])
		if len(urls) == 0 {
			return "urls 不能为空"
		}
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

func stringList(value any) []string {
	var list []string
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				list = append(list, strings.TrimSpace(text))
			}
		}
	case []string:
		list = typed
	case string:
		if typed != "" {
			list = []string{typed}
		}
	}
	return list
}

// complete 请求模型一次，收齐文字、工具调用与 token 数。
func complete(ctx context.Context, model brain.ChatModel, messages []brain.ChatMessage, specs []brain.ToolSpec) (string, []brain.ToolCall, int, error) {
	var text strings.Builder
	var calls []brain.ToolCall
	tokens := 0
	for delta, err := range model.StreamReply(ctx, messages, specs) {
		if err != nil {
			return text.String(), calls, tokens, err
		}
		text.WriteString(delta.Text)
		calls = append(calls, delta.ToolCalls...)
		if delta.Usage != nil {
			tokens += delta.Usage.PromptTokens + delta.Usage.OutputTokens
		}
	}
	return text.String(), calls, tokens, nil
}

// splitSources 把「结论 …… 来源：网址……」拆开：结论会被念出来，来源只存档。
func splitSources(text string) (string, []string) {
	text = strings.TrimSpace(text)
	index := strings.LastIndex(text, "来源：")
	if index < 0 {
		index = strings.LastIndex(text, "来源:")
	}
	if index < 0 {
		return text, nil
	}
	var sources []string
	separators := func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("，,；;、()（）<>「」", r) }
	for _, field := range strings.FieldsFunc(text[index:], separators) {
		if i := strings.Index(field, "http"); i >= 0 {
			sources = append(sources, field[i:])
		}
	}
	return strings.TrimSpace(text[:index]), sources
}

var researchToolSpecs = []brain.ToolSpec{
	{Name: "search_web", Description: "联网搜索，返回几条结果（标题、网址、摘要）。",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"搜索词，简洁具体"}},"required":["query"]}`)},
	{Name: "read_pages", Description: "读网页正文（一次最多 3 个网址，每页只给开头一部分）。摘要不够用时才读。",
		Parameters: json.RawMessage(`{"type":"object","properties":{"urls":{"type":"array","items":{"type":"string"},"description":"要读的网址，取自搜索结果"}},"required":["urls"]}`)},
}

func researchPrompt(limits Limits, now time.Time) string {
	// 告诉模型今天的日期（2026-11-17 实测：不说的话搜索词里写的是两年前的年份，搜回来的都是旧资料）
	today := fmt.Sprintf("今天是 %s（%s）。要查「现在」「最近」的事时按这个日期来，不要用别的年份。\n", now.Format("2006年1月2日"), weekdayNames[now.Weekday()])
	return today + fmt.Sprintf(`你是调查助手，在后台替主人查资料。用 search_web 搜索、必要时用 read_pages 读正文，查清楚后直接给出结论。
限制：最多搜索 %d 次、最多 %d 轮；够用就停，不要为了凑数多查。搜索词按资料最可能使用的语言写（日本的事用日语搜，其他按情况用英语或中文）。
最后的回答格式：先用中文口语写 3 到 6 句结论（会被念出来：不要列表、表格、网址、Markdown），
有不确定或没查到的地方直接说；然后另起一行写「来源：」，列出 1 到 3 个最主要的网址。`, limits.MaxSearches, limits.MaxRounds)
}

var weekdayNames = []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}
