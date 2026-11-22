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

// 调查的过程：调查的模型决定搜什么、读哪几页，查够了交结论；到上限就不再给工具，逼它用已有资料写。

// 调查过程里写死的几项（都不开放成配置）。
const (
	// writeUpTimeout 是到上限后写结论最多等多久（不算在搜索阶段里）：一次不带搜索工具、输出不长的请求，一般几秒就完；
	// 也算进告诉主人的最长等待（LongestWait）。是请求要的时间，不是部署时要调的参数。
	writeUpTimeout = 45 * time.Second
	// searchResults 是调查时每次搜索要几条结果：多一条，之后每一轮都多带几百字的输入；5 条是对比四种调查做法的试验时
	// 用的数（Tavily 基础搜索不论几条都是 1 积分），改它要重新对比质量和花费。
	searchResults = 5
	// pageRunes 是读网页时每页最多给模型多少字（按调查的问题挑相关的段落）：读得越多，之后每一轮越贵；
	// 2,000 字同样是试验时用的数，改它要重新对比，不是部署时调的参数。
	pageRunes = 2000
	// maxPagesPerRead 是一次最多读几页：读得越多这一步越贵；多给的网址告诉模型没读，需要的话它再分批读。
	maxPagesPerRead = 3
)

// investigationReport 是一次调查的经过和结果。
type investigationReport struct {
	status          Status
	conclusion      string
	sources         []string
	reason          string
	rounds          int
	searches, reads int    // 调用了几次（没成功的也算：搜索次数的上限按它算）
	gathered        int    // 成功拿到的资料份数（搜索成功、读网页成功）：一份都没有就不交出结论
	lookupFailure   string // 最近一次搜索、读网页为什么没成功（一份资料都没拿到时写进原因）
	tokens          int
}

// noMaterialYet 是一份资料都没拿到就想交结论时退回给模型的话。
var noMaterialYet = "还没有拿到任何资料（搜索、读网页都还没成功过）。这是调查任务，结论必须以查到的资料为准：先用 " +
	searchWebSpec.Name + " 搜，再交结论。"

// conclusionNotTaken 是同一步里既查资料又交结论时，给那次 submit_conclusion 的结果：先看查到的，再交。
var conclusionNotTaken = "这一步同时在查资料，结论没收下：看完这一步查到的结果，再用 " + submitConclusionSpec.Name + " 交结论。"

// investigate 是调查的主循环：模型决定搜什么、读哪几页；到任何一个上限就不再给工具，逼它用已有材料写结论。
// 一份资料都没拿到时不交出结论（模型凭印象的话不能当成调查结果）。
func (r *Runner) investigate(ctx context.Context, question string) investigationReport {
	searchCtx, cancel := context.WithTimeout(ctx, r.limits.Timeout)
	defer cancel()
	messages := []brain.ChatMessage{
		{Role: brain.RoleSystem, Text: researchPrompt(r.limits, time.Now())},
		{Role: brain.RoleUser, Text: question},
	}
	var report investigationReport
	stop := ""
	nudged := false // 已经因为「一份资料都没有就交结论」退回过一次
	for {
		switch {
		case report.rounds >= r.limits.MaxRounds:
			stop = fmt.Sprintf("已经查了 %d 轮（上限）", report.rounds)
		case report.tokens >= r.limits.MaxTokens:
			stop = fmt.Sprintf("用了约 %d token（上限 %d）", report.tokens, r.limits.MaxTokens)
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
		report.rounds++
		text, calls, tokens, err := askModel(searchCtx, r.model, messages, researchToolSpecs)
		report.tokens += tokens
		if err != nil {
			if ctx.Err() != nil {
				return report
			}
			if searchCtx.Err() != nil {
				continue // 下一圈判成时间上限
			}
			if report.gathered > 0 { // 已经查到资料：不丢掉，和到上限一样用它们写结论
				stop = "调查模型出错：" + err.Error()
				break
			}
			report.status, report.reason = StatusFailed, "模型出错："+err.Error()
			return report
		}
		for i := range calls {
			if calls[i].ID == "" {
				calls[i].ID = fmt.Sprintf("r%d-%d", report.rounds, i)
			}
		}
		if hasLookup(calls) { // 这一步在查资料：先执行；同一步交的结论不算（看到结果再交）
			messages = append(messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text, ToolCalls: calls})
			for _, call := range calls {
				result := conclusionNotTaken
				if call.Name != submitConclusionSpec.Name {
					result = r.runTool(searchCtx, call, question, &report)
				}
				messages = append(messages, toolResult(call, result))
			}
			continue
		}
		draft, submitted, err := submittedConclusion(calls)
		if err != nil { // submit_conclusion 的参数不对：说明哪里不对，让它重新交
			r.logger.Warn("submit_conclusion 的参数不对", "err", err)
			messages = append(messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text, ToolCalls: calls})
			for _, call := range calls {
				messages = append(messages, toolResult(call, failureText(err)))
			}
			continue
		}
		if report.gathered == 0 {
			// 一份资料都没拿到就想交差（真机上见过：云端模型凭印象 1 秒就给了结论）：退回一次，要求先搜；
			// 退回后还是没拿到资料，就按没查到资料收尾，不把凭印象的话当成调查结果
			if nudged {
				report.status, report.reason = StatusFailed, report.noMaterialReason()
				return report
			}
			nudged = true
			messages = append(messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text, ToolCalls: calls})
			if len(calls) == 0 {
				messages = append(messages, brain.ChatMessage{Role: brain.RoleUser, Text: noMaterialYet})
			}
			for _, call := range calls {
				messages = append(messages, toolResult(call, noMaterialYet))
			}
			continue
		}
		// 模型认为查够了，交出了结论
		report.status = StatusDone
		if !submitted { // 没用 submit_conclusion、直接写了文字：整段当结论（没有单独的来源）
			draft = conclusionDraft{Conclusion: strings.TrimSpace(text)}
			r.logger.Warn("调查模型没用 submit_conclusion 交结论：整段文字当结论")
		}
		if r.writer != nil { // 交给写结论的模型重写（不带调查模型的草稿，免得被它带偏）；写不成就用草稿
			instruction := brain.ChatMessage{Role: brain.RoleUser,
				Text: "资料已经查够了。不要再搜索，根据上面查到的资料写结论，用 " + submitConclusionSpec.Name + " 交出来。"}
			if written, err := r.writeConclusion(ctx, append(messages, instruction), &report, r.writer); err == nil {
				draft = written
			} else if ctx.Err() == nil {
				r.logger.Warn("写结论的模型出错，用调查模型的草稿", "err", err)
			}
		}
		report.conclusion, report.sources = draft.Conclusion, draft.Sources
		if report.conclusion == "" {
			report.status, report.reason = StatusFailed, "模型没有给出结论"
		}
		return report
	}

	if report.gathered == 0 { // 到上限（或模型出错）时一份资料都没有：没有可写的
		report.status, report.reason = StatusFailed, stop+"；"+report.noMaterialReason()
		return report
	}
	// 到上限：不再给工具，用已有材料写结论（写结论的时间单算；超出的这一点费用在额度上允许）
	messages = append(messages, brain.ChatMessage{Role: brain.RoleUser,
		Text: "调查到此为止（" + stop + "）。不要再搜索，根据上面已经查到的资料写结论，用 " + submitConclusionSpec.Name +
			" 交出来；没查清的地方明确说没查完。"})
	writers := []brain.ChatModel{r.model}
	if r.writer != nil {
		writers = []brain.ChatModel{r.writer, r.model} // 写结论的模型出错时由调查模型写
	}
	draft, err := r.writeConclusion(ctx, messages, &report, writers...)
	report.reason = stop
	if err != nil {
		if ctx.Err() != nil {
			return report
		}
		report.status = StatusFailed
		report.reason = stop + "；写结论时出错：" + err.Error()
		return report
	}
	report.status = StatusPartial
	report.conclusion, report.sources = draft.Conclusion, draft.Sources
	return report
}

// noMaterialReason 是一份资料都没拿到时的原因：带上最近一次没查成的说明（例如今天的搜索额度用完了）。
func (report *investigationReport) noMaterialReason() string {
	if report.lookupFailure == "" {
		return "没查到资料"
	}
	return "没查到资料：" + report.lookupFailure
}

// hasLookup 判断这一步有没有交结论以外的调用（搜索、读网页，或者叫错名字的工具：照样执行，把结果交给模型）。
func hasLookup(calls []brain.ToolCall) bool {
	for _, call := range calls {
		if call.Name != submitConclusionSpec.Name {
			return true
		}
	}
	return false
}

func toolResult(call brain.ToolCall, text string) brain.ChatMessage {
	return brain.ChatMessage{Role: brain.RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: text}
}

// writeConclusion 依次让 writers 根据 messages 里的资料写结论（只给 submit_conclusion），第一个写成的为准；
// 每个模型各有 writeUpTimeout。没用工具、直接写了文字的，整段当结论。
func (r *Runner) writeConclusion(ctx context.Context, messages []brain.ChatMessage, report *investigationReport,
	writers ...brain.ChatModel) (conclusionDraft, error) {
	var lastErr error
	for i, writer := range writers {
		writeCtx, cancel := context.WithTimeout(ctx, writeUpTimeout)
		text, calls, tokens, err := askModel(writeCtx, writer, messages, []brain.ToolSpec{submitConclusionSpec})
		cancel()
		report.tokens += tokens
		if err == nil {
			draft, submitted, decodeErr := submittedConclusion(calls)
			if decodeErr != nil {
				r.logger.Warn("写结论的模型给的 submit_conclusion 参数不对", "err", decodeErr)
			}
			if submitted && draft.Conclusion != "" {
				return draft, nil
			}
			if text = strings.TrimSpace(text); text != "" {
				r.logger.Warn("写结论的模型没用 submit_conclusion：整段文字当结论")
				return conclusionDraft{Conclusion: text}, nil
			}
			err = errors.Join(errors.New("没写出结论"), decodeErr)
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
	Conclusion string   `json:"conclusion" jsonschema:"3 到 6 句口语的结论，用和问题相同的语言写（会被念出来：不要列表、表格、网址、Markdown）；有不确定或没查到的地方直接说"`
	Sources    []string `json:"sources,omitempty" jsonschema:"1 到 3 个最主要的来源网址"`
}

// submittedConclusion 找出这一步里的 submit_conclusion 调用并解出参数：没有这个调用时 submitted = false；
// 参数不对时返回错误（说明哪里不对）。
func submittedConclusion(calls []brain.ToolCall) (draft conclusionDraft, submitted bool, err error) {
	for _, call := range calls {
		if call.Name != submitConclusionSpec.Name {
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

// runTool 执行调查里的一次搜索或读网页，返回给模型看的文字（出错也作为文字交给模型，让它换个办法）。
// 成功拿到的才算资料（report.gathered）；没成功的记下原因。
func (r *Runner) runTool(ctx context.Context, call brain.ToolCall, question string, report *investigationReport) string {
	switch call.Name {
	case searchWebSpec.Name:
		if report.searches >= r.limits.MaxSearches {
			return fmt.Sprintf("搜索次数已经用完（上限 %d 次）。请根据已有资料写结论。", r.limits.MaxSearches)
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
		return report.lookupResult(searchOutput{Results: hits}, err)
	case readPagesSpec.Name:
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
			pages, err = r.searcher.ReadPages(ctx, urls, pageRunes, question, r.meter)
			return err
		})
		report.reads++
		r.logger.Info("调查：读网页", "urls", len(urls), "skipped", len(skipped), "err", err)
		out := readPagesOutput{Pages: pages}
		if len(skipped) > 0 { // 多给的网址没读：告诉模型是哪几个，需要的话它再分批读
			out.NotRead = skipped
			out.Note = fmt.Sprintf("一次最多读 %d 个网址，not_read 里的这次没读；需要的话再调用一次 %s 读它们。",
				maxPagesPerRead, readPagesSpec.Name)
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

// 调查用的三个工具：提示词、退回的话里提到它们时都从这里取名字。
var (
	searchWebSpec = brain.ToolSpec{Name: "search_web", Description: "联网搜索，返回几条结果（标题、网址、摘要）。",
		Parameters: brain.ParametersFor[searchInput]()}
	readPagesSpec = brain.ToolSpec{Name: "read_pages",
		Description: fmt.Sprintf("读网页正文（一次最多 %d 个网址；每页给出和调查的问题相关的段落）。摘要不够用时才读。", maxPagesPerRead),
		Parameters:  brain.ParametersFor[readPagesInput]()}
	submitConclusionSpec = brain.ToolSpec{Name: "submit_conclusion",
		Description: "交出调查结论（结论和来源分开）。查够了就调用它，不要把结论直接写在回答里。",
		Parameters:  brain.ParametersFor[conclusionDraft]()}
	researchToolSpecs = []brain.ToolSpec{searchWebSpec, readPagesSpec, submitConclusionSpec}
)

func researchPrompt(limits Limits, now time.Time) string {
	// 告诉模型今天的日期（2026-11-17 实测：不说的话搜索词里写的是两年前的年份，搜回来的都是旧资料）
	today := fmt.Sprintf("今天是 %s（%s）。要查「现在」「最近」的事时按这个日期来，不要用别的年份。\n", now.Format("2006年1月2日"), brain.WeekdayName(now))
	return today + fmt.Sprintf(`你是调查助手，在后台替主人查资料。用 %s 搜索、必要时用 %s 读正文，查清楚后直接给出结论。
限制：最多搜索 %d 次、最多 %d 轮；够用就停，不要为了凑数多查。搜索词按资料所在的地区和语言写（那里的资料多半用什么语言，就用什么语言搜），不一定和问题的语言相同。
查清楚后调用 %s 交出结论和来源。`, searchWebSpec.Name, readPagesSpec.Name, limits.MaxSearches, limits.MaxRounds, submitConclusionSpec.Name)
}
