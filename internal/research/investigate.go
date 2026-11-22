package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// 调查的过程：调查的模型决定搜什么、读哪几页，查够了交结论草稿；到上限就不再给搜索工具，逼它用已有资料写。
// 草稿交出去之前要不要审查、审查之后怎么办，见 review.go。

// 调查过程里写死的几项（都不开放成配置）。
const (
	// writeUpTimeout 是到上限后写结论最多等多久（不算在搜索阶段里）：一次不带搜索工具、输出不长的请求，一般几秒就完；
	// 也算进告诉主人的最长等待（LongestWait）。是请求要的时间，不是部署时要调的参数。
	writeUpTimeout = 45 * time.Second
	// searchResults 是调查时每次搜索要几条结果：多一条，之后每一轮都多带几百字的输入；5 条是对比四种调查做法的试验时
	// 用的数（Tavily 基础搜索不论几条都是 1 积分），改它要重新对比质量和花费。
	searchResults = 5
	// pageRunes 是读网页时每页最多给模型多少字（按调查的问题挑相关的段落）：读得越多，之后每一轮越贵；审查时每个来源给的
	// 摘录也是这么长（maxExcerptRunes：审查看到的就是调查时看到的）。2,000 字同样是试验时用的数，改它就同时改了调查和审查的
	// 花费与质量，要重新对比，不是部署时调的参数。
	pageRunes = 2000
	// maxPagesPerRead 是一次最多读几页：读得越多这一步越贵；多给的网址告诉模型没读，需要的话它再分批读。
	maxPagesPerRead = 3
)

// investigationReport 是一次调查的经过和结果。
type investigationReport struct {
	status          Status
	conclusion      string
	sources         []string
	reason          string // 为什么没查完 / 为什么失败；没审查成、审查的疑点没能复查时也写在这里
	rounds          int    // 搜索阶段请求了几轮（轮数的上限按它算）
	searches, reads int    // 调用了几次（没成功的也算：搜索次数的上限按它算）
	gathered        int    // 成功拿到的资料份数（搜索成功、读网页成功）：一份都没有就不交出结论
	lookupFailure   string // 最近一次搜索、读网页为什么没成功（一份资料都没拿到时写进原因）
	tokens          int    // 调查的模型用的 token（搜索阶段、写结论、复查）
	// 审查（日志用）：结果、疑点几处、复查了几轮、审查用的 token
	review        reviewOutcome
	doubts        int
	recheckRounds int
	reviewTokens  int
}

// inquiry 是一个调查任务的对话与经过：调查的模型看到的消息、请求了几次、按网址记下的资料、结果。
type inquiry struct {
	r        *Runner
	question string
	messages []brain.ChatMessage
	steps    int // 请求了调查的模型几次（给没编号的工具调用编号）
	sources  sourceNotes
	report   investigationReport
}

// investigate 查一个问题：先查资料、交草稿（gather）；有审查时草稿交给审查，有疑点就退回复查一次（settle）。
func (r *Runner) investigate(ctx context.Context, question string) investigationReport {
	now := r.now()
	q := &inquiry{r: r, question: question, sources: sourceNotes{}, report: investigationReport{review: reviewNone},
		messages: []brain.ChatMessage{
			{Role: brain.RoleSystem, Text: r.agents.Investigator.Instructions},
			{Role: brain.RoleUser, Text: taskMessage(now, question)},
		}}
	if draft, ok := q.gather(ctx); ok {
		q.settle(ctx, draft, now)
	}
	return q.report
}

// taskMessage 是交给调查的模型的任务：现在的日期和要调查的问题。日期每天不同，放在消息里，系统提示词逐字不变
// （不告诉日期的话，搜索词里会写成两年前的年份，搜回来的都是旧资料：实测）。
func taskMessage(now time.Time, question string) string {
	return "现在是 " + brain.DateText(now) + "。\n要调查的问题：" + question
}

// noMaterialYet 是一份资料都没拿到就想交结论时退回给模型的话。
var noMaterialYet = "还没有拿到任何资料（搜索、读网页都还没成功过）。这是调查任务，结论必须以查到的资料为准：先用 " +
	searchWebTool + " 搜，再交结论。"

// conclusionNotTaken 是同一步里既查资料又交结论时，给那次 submit_conclusion 的结果：先看查到的，再交。
var conclusionNotTaken = "这一步同时在查资料，结论没收下：看完这一步查到的结果，再用 " + submitConclusionTool + " 交结论。"

// gather 是搜索阶段：调查的模型决定搜什么、读哪几页，直到它交出结论（草稿）；到任何一个上限就不再给搜索工具，逼它用已有资料写。
// 一份资料都没拿到时不交出结论（模型凭印象的话不能当成调查结果）。ok = 有草稿；没有时 report 里记着为什么（被取消时不记）。
func (q *inquiry) gather(ctx context.Context) (conclusionDraft, bool) {
	limits, maxRounds := q.r.limits, q.r.agents.Investigator.MaxSteps
	report := &q.report
	searchCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	stop := ""
	nudged := false // 已经因为「一份资料都没有就交结论」退回过一次
search:
	for {
		switch {
		case report.rounds >= maxRounds:
			stop = fmt.Sprintf("已经查了 %d 轮（上限）", report.rounds)
		case report.tokens >= limits.MaxTokens:
			stop = fmt.Sprintf("用了约 %d token（上限 %d）", report.tokens, limits.MaxTokens)
		case searchCtx.Err() != nil:
			stop = fmt.Sprintf("查了 %s（时间上限）", limits.Timeout)
		}
		if stop == "" {
			if err := q.r.budget.AllowResearch(ctx); err != nil {
				stop = err.Error()
			}
		}
		if stop != "" {
			break
		}
		report.rounds++
		draft, concluded, err := q.step(searchCtx, q.r.investigationSpecs, limits.MaxSearches)
		switch {
		case err != nil && ctx.Err() != nil:
			return conclusionDraft{}, false
		case err != nil && searchCtx.Err() != nil:
			continue // 下一圈判成时间上限
		case err != nil && report.gathered > 0: // 已经查到资料：不丢掉，和到上限一样用它们写结论
			stop = "调查模型出错：" + err.Error()
			break search
		case err != nil:
			report.status, report.reason = StatusFailed, "模型出错："+err.Error()
			return conclusionDraft{}, false
		case !concluded:
			continue
		case report.gathered == 0:
			// 一份资料都没拿到就想交差（真机上见过：云端模型凭印象 1 秒就给了结论）：退回一次，要求先搜；
			// 退回后还是没拿到资料，就按没查到资料收尾，不把凭印象的话当成调查结果
			if nudged {
				report.status, report.reason = StatusFailed, report.noMaterialReason()
				return conclusionDraft{}, false
			}
			nudged = true
			q.reply(noMaterialYet)
			continue
		case draft.Conclusion == "":
			report.status, report.reason = StatusFailed, "模型没有给出结论"
			return conclusionDraft{}, false
		}
		report.status = StatusDone // 模型认为查够了，交出了结论
		return draft, true
	}

	if report.gathered == 0 { // 到上限（或模型出错）时一份资料都没有：没有可写的
		report.status, report.reason = StatusFailed, stop+"；"+report.noMaterialReason()
		return conclusionDraft{}, false
	}
	// 到上限：不再给搜索工具，用已有材料写结论（写结论的时间单算；超出的这一点费用在额度上允许）
	draft, err := q.writeUp(ctx, stop)
	report.reason = stop
	if err != nil {
		if ctx.Err() != nil {
			return conclusionDraft{}, false
		}
		report.status, report.reason = StatusFailed, stop+"；写结论时出错："+err.Error()
		return conclusionDraft{}, false
	}
	report.status = StatusPartial
	return draft, true
}

// step 请求调查的模型一次（offer 是这一步给的工具），处理它这一步的调用：查资料的照常执行（这一步同时交的结论不算，看到结果
// 再交）；交的结论参数不对时说明哪里不对，让它重交。交出了结论时 concluded = true（没用工具、直接写了文字的整段当结论）。
// 模型这一步说的话（和调用）接在消息后面；交出结论的那次调用还没回结果，由调用方回（reply、answerCalls）。
func (q *inquiry) step(ctx context.Context, offer []brain.ToolSpec, searchCap int) (draft conclusionDraft, concluded bool, err error) {
	q.steps++
	text, calls, tokens, err := askModel(ctx, q.r.agents.Investigator.Model, q.messages, offer)
	q.report.tokens += tokens
	if err != nil {
		return conclusionDraft{}, false, err
	}
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("r%d-%d", q.steps, i)
		}
	}
	q.messages = append(q.messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text, ToolCalls: calls})
	if hasLookup(calls) {
		for _, call := range calls {
			result := conclusionNotTaken
			if call.Name != submitConclusionTool {
				result = q.runTool(ctx, call, searchCap)
			}
			q.messages = append(q.messages, toolResult(call, result))
		}
		return conclusionDraft{}, false, nil
	}
	draft, submitted, err := submittedConclusion(calls)
	if err != nil {
		q.r.logger.Warn("submit_conclusion 的参数不对", "err", err)
		q.answerCalls(failureText(err))
		return conclusionDraft{}, false, nil
	}
	if !submitted { // 没用 submit_conclusion、直接写了文字：整段当结论（没有单独的来源）
		draft = conclusionDraft{Conclusion: strings.TrimSpace(text)}
		q.r.logger.Warn("调查模型没用 submit_conclusion 交结论：整段文字当结论")
	}
	return draft, true, nil
}

// writeUp 到上限后让调查的模型用已有的资料写结论：不再给搜索工具，只给 submit_conclusion；写结论的时间单算（writeUpTimeout）。
// 没用工具、直接写了文字的整段当结论。
func (q *inquiry) writeUp(ctx context.Context, stop string) (conclusionDraft, error) {
	q.messages = append(q.messages, brain.ChatMessage{Role: brain.RoleUser,
		Text: "调查到此为止（" + stop + "）。不要再搜索，根据上面已经查到的资料写结论，用 " + submitConclusionTool +
			" 交出来；没查清的地方明确说没查完。"})
	writeCtx, cancel := context.WithTimeout(ctx, writeUpTimeout)
	defer cancel()
	q.steps++
	text, calls, tokens, err := askModel(writeCtx, q.r.agents.Investigator.Model, q.messages, q.r.submitSpecs)
	q.report.tokens += tokens
	if err != nil {
		return conclusionDraft{}, err
	}
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("r%d-%d", q.steps, i)
		}
	}
	q.messages = append(q.messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text, ToolCalls: calls})
	draft, submitted, decodeErr := submittedConclusion(calls)
	if decodeErr != nil {
		q.r.logger.Warn("写结论时给的 submit_conclusion 参数不对", "err", decodeErr)
	}
	if submitted && draft.Conclusion != "" {
		return draft, nil
	}
	if text = strings.TrimSpace(text); text != "" {
		q.r.logger.Warn("写结论时没用 submit_conclusion：整段文字当结论")
		return conclusionDraft{Conclusion: text}, nil
	}
	return conclusionDraft{}, errors.Join(errors.New("没写出结论"), decodeErr)
}

// reply 回应调查的模型上一步：它调用了工具的，每个调用回 text；没调用工具的，回一条用户消息。
func (q *inquiry) reply(text string) {
	if last := q.messages[len(q.messages)-1]; last.Role == brain.RoleAssistant && len(last.ToolCalls) > 0 {
		q.answerCalls(text)
		return
	}
	q.messages = append(q.messages, brain.ChatMessage{Role: brain.RoleUser, Text: text})
}

// answerCalls 给上一步（最后一条助手消息）的每个工具调用回 text（每个调用后面都要紧跟着它的结果）；没有调用就什么都不回。
func (q *inquiry) answerCalls(text string) {
	last := q.messages[len(q.messages)-1]
	if last.Role != brain.RoleAssistant {
		return
	}
	for _, call := range last.ToolCalls {
		q.messages = append(q.messages, toolResult(call, text))
	}
}

// noMaterialReason 是一份资料都没拿到时的原因：带上最近一次没查成的说明（例如今天的搜索额度用完了）。
func (report *investigationReport) noMaterialReason() string {
	if report.lookupFailure == "" {
		return "没查到资料"
	}
	return "没查到资料：" + report.lookupFailure
}

// hasLookup 判断这一步有没有交结论以外的调用（搜索、读网页，或者叫错名字的工具：照样回结果，告诉模型没有这个工具）。
func hasLookup(calls []brain.ToolCall) bool {
	for _, call := range calls {
		if call.Name != submitConclusionTool {
			return true
		}
	}
	return false
}

func toolResult(call brain.ToolCall, text string) brain.ChatMessage {
	return brain.ChatMessage{Role: brain.RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: text}
}

// conclusionDraft 是模型用 submit_conclusion 交出的结论：结论会被念出来，来源只存档（有审查时，审查只看这几个来源的摘录）。
type conclusionDraft struct {
	Conclusion string   `json:"conclusion" jsonschema:"3 到 6 句口语的结论，用和问题相同的语言写（会被念出来：不要列表、表格、网址、Markdown）；有不确定或没查到的地方直接说"`
	Sources    []string `json:"sources,omitempty" jsonschema:"结论里的说法出自哪些网址（最主要的在前，一般 1 到 3 个）：具体的数字、日期、名字、价格要能在这几个来源里找到"`
}

// submittedConclusion 找出这一步里的 submit_conclusion 调用并解出参数：没有这个调用时 submitted = false；
// 参数不对时返回错误（说明哪里不对）。
func submittedConclusion(calls []brain.ToolCall) (draft conclusionDraft, submitted bool, err error) {
	for _, call := range calls {
		if call.Name != submitConclusionTool {
			continue
		}
		draft, err := brain.DecodeArguments[conclusionDraft](call.Arguments)
		if err != nil {
			return conclusionDraft{}, false, fmt.Errorf("submit_conclusion 的参数不对: %w", err)
		}
		draft.Conclusion = strings.TrimSpace(draft.Conclusion)
		return draft, true, nil
	}
	return conclusionDraft{}, false, nil
}

// runTool 执行调查里的一次搜索或读网页，返回给模型看的文字（出错也作为文字交给模型，让它换个办法）。只执行调查的代理能用的
// 工具（没交给它的照着名字调用也不执行）；搜索最多搜到第 searchCap 次。成功拿到的才算资料（report.gathered），按网址记下
// （审查时给摘录用）；没成功的记下原因。
func (q *inquiry) runTool(ctx context.Context, call brain.ToolCall, searchCap int) string {
	r, report := q.r, &q.report
	if !allows(r.agents.Investigator, call.Name) {
		return "没有叫 " + call.Name + " 的工具"
	}
	switch call.Name {
	case searchWebTool:
		if report.searches >= searchCap {
			return fmt.Sprintf("搜索次数已经用完（上限 %d 次）。请根据已有资料写结论。", searchCap)
		}
		in, err := brain.DecodeArguments[searchInput](call.Arguments)
		if err != nil || strings.TrimSpace(in.Query) == "" {
			return failureText(errors.Join(errors.New("query 不能为空"), err))
		}
		var hits []SearchHit
		err = r.withinBudget(ctx, r.searcher.SearchCredits(), func() (err error) {
			hits, err = r.searcher.Search(ctx, in.Query, searchResults, r.meter)
			return err
		})
		report.searches++
		r.logger.Info("调查：搜索", "query", in.Query, "results", len(hits), "err", err)
		if err == nil {
			q.sources.addHits(hits)
		}
		return report.lookupResult(searchOutput{Results: hits}, err)
	case readPagesTool:
		in, err := brain.DecodeArguments[readPagesInput](call.Arguments)
		if err != nil || len(in.URLs) == 0 {
			return failureText(errors.Join(errors.New("urls 不能为空"), err))
		}
		urls, skipped := in.URLs, []string(nil)
		if len(urls) > maxPagesPerRead {
			urls, skipped = urls[:maxPagesPerRead], urls[maxPagesPerRead:]
		}
		var pages []Page
		err = r.withinBudget(ctx, r.searcher.ReadCredits(len(urls)), func() (err error) {
			pages, err = r.searcher.ReadPages(ctx, urls, pageRunes, q.question, r.meter)
			return err
		})
		report.reads++
		r.logger.Info("调查：读网页", "urls", len(urls), "skipped", len(skipped), "err", err)
		if err == nil {
			q.sources.addPages(pages)
		}
		out := readPagesOutput{Pages: pages}
		if len(skipped) > 0 { // 多给的网址没读：告诉模型是哪几个，需要的话它再分批读
			out.NotRead = skipped
			out.Note = fmt.Sprintf("一次最多读 %d 个网址，not_read 里的这次没读；需要的话再调用一次 %s 读它们。",
				maxPagesPerRead, readPagesTool)
		}
		return report.lookupResult(out, err)
	default:
		return "没有叫 " + call.Name + " 的工具"
	}
}

// searchOutput、readPagesOutput 是搜索、读网页交给模型的结果。
type searchOutput struct {
	Results []SearchHit `json:"results"`
}

type readPagesOutput struct {
	Pages   []Page   `json:"pages"`
	NotRead []string `json:"not_read,omitempty"` // 超过一次能读的个数、这次没读的网址
	Note    string   `json:"note,omitempty"`
}

// lookupResult 记下一次搜索或读网页的结果：成功的算一份资料，没成功的记下原因；返回给模型看的文字（结果编成 JSON）。
func (report *investigationReport) lookupResult(result any, err error) string {
	var text []byte
	if err == nil {
		text, err = json.Marshal(result)
	}
	if err != nil {
		report.lookupFailure = err.Error()
		return failureText(err)
	}
	report.gathered++
	return string(text)
}

// withinBudget 先查额度再执行。记账在搜索那一层：发出去的每次请求不管成没成功都记（run 里把 r.meter 交给它，见 Searcher）。
func (r *Runner) withinBudget(ctx context.Context, credits int, run func() error) error {
	if err := r.meter.AllowSearch(ctx, credits); err != nil {
		return err
	}
	return run()
}

func failureText(err error) string {
	return "失败：" + err.Error()
}

// askModel 请求模型一次，收齐文字、工具调用与 token 数（出错时也算上知道的用量）；被截断按失败（brain.ErrTruncated）。
func askModel(ctx context.Context, model brain.ChatModel, messages []brain.ChatMessage, specs []brain.ToolSpec) (string, []brain.ToolCall, int, error) {
	reply, err := brain.CollectReply(ctx, model, brain.ChatRequest{Messages: messages, Tools: specs})
	return reply.Text, reply.ToolCalls, reply.Usage.PromptTokens + reply.Usage.OutputTokens, err
}

type searchInput struct {
	Query string `json:"query" jsonschema:"搜索词，简洁具体"`
}

type readPagesInput struct {
	URLs []string `json:"urls" jsonschema:"要读的网址，取自搜索结果"`
}

// 调查的代理用的三个工具（名字见 agents.go）。
var (
	searchWebSpec = brain.ToolSpec{Name: searchWebTool, Description: "联网搜索，返回几条结果（标题、网址、摘要）。",
		Parameters: brain.ParametersFor[searchInput]()}
	readPagesSpec = brain.ToolSpec{Name: readPagesTool,
		Description: fmt.Sprintf("读网页正文（一次最多 %d 个网址；每页给出和调查的问题相关的段落）。摘要不够用时才读。", maxPagesPerRead),
		Parameters:  brain.ParametersFor[readPagesInput]()}
	submitConclusionSpec = brain.ToolSpec{Name: submitConclusionTool,
		Description: "交出调查结论（结论和来源分开）。查够了就调用它，不要把结论直接写在回答里。",
		Parameters:  brain.ParametersFor[conclusionDraft]()}
)
