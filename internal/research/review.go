package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/bukahou/haro/internal/brain"
)

// 审查：草稿交给更强的模型核对，只给问题、草稿和草稿列出的来源的摘录（不给调查过程）；它用 submit_review 交回通过或疑点，
// 只审、不改结论。有疑点时退回原来的调查对话复查一次，由调查的模型自己改。代码不改结论的文字，也不看疑点说了什么：只按结构走。

// 复查的额外额度（审查只审一次，复查也只有一次）。
const (
	// recheckSearches 是复查时最多再搜几次：复查是核对已有的说法，不是重新调查；对比试验里审查每题挑出 1–4 处，
	// 一次搜索常常能核对好几处，读网页不另限次数（受轮数管）。
	recheckSearches = 2
	// recheckRounds 是复查时最多请求调查的模型几轮：搜两次、读一两次，再留一轮交结论。
	recheckRounds = 4
)

// reviewMaterial 是交给审查的全部内容（编成 JSON 放在一条消息里）。现在的日期每次不同，放在这里，系统提示词逐字不变。
type reviewMaterial struct {
	Today    string          `json:"today"`
	Question string          `json:"question"`
	Draft    string          `json:"draft"`
	Sources  []sourceExcerpt `json:"sources"`
}

// verdictKind 是审查的结论；doubtKind 是疑点的问题种类。取值只在这里定义：参数定义（给模型看的枚举）和交回后的核对都按它。
type (
	verdictKind string
	doubtKind   string
)

const (
	verdictPass   verdictKind = "pass"
	verdictDoubts verdictKind = "doubts"
)

const (
	doubtUnsupported  doubtKind = "unsupported"
	doubtContradicted doubtKind = "contradicted"
	doubtInaccurate   doubtKind = "inaccurate"
)

// doubtKinds 是问题种类：meaning 写进参数定义给审查看，label 是交回给调查的模型时的说法。
var doubtKinds = []struct {
	kind           doubtKind
	label, meaning string
}{
	{doubtUnsupported, "没依据", "摘录里找不到依据"},
	{doubtContradicted, "和资料矛盾", "和摘录说的矛盾"},
	{doubtInaccurate, "不准确", "摘录里有，但草稿说得不准确（数字、日期、范围、名字对不上）"},
}

// doubtLabel 是问题种类交回给调查的模型时的说法；不认识的种类 ok = false。
func doubtLabel(kind doubtKind) (label string, ok bool) {
	for _, known := range doubtKinds {
		if known.kind == kind {
			return known.label, true
		}
	}
	return "", false
}

// reviewVerdict 是审查用 submit_review 交回的。
type reviewVerdict struct {
	Verdict verdictKind   `json:"verdict"`
	Doubts  []reviewDoubt `json:"doubts,omitempty" jsonschema:"有疑点时逐条列出；通过时不填"`
}

type reviewDoubt struct {
	Claim    string    `json:"claim" jsonschema:"草稿里有问题的那句话或那个说法（照草稿原样摘出来）"`
	Kind     doubtKind `json:"kind"`
	Evidence string    `json:"evidence" jsonschema:"依据说的是什么：摘录里相关的原话或说法；没依据时写摘录里有什么、缺什么"`
}

// check 照参数定义核对交回的审查：结论和问题种类是枚举里的值；pass 不带疑点，doubts 至少一条，每条要写出是哪句话。
func (v reviewVerdict) check() error {
	switch v.Verdict {
	case verdictPass:
		if len(v.Doubts) > 0 {
			return errors.New("verdict 是 pass，却列了疑点")
		}
	case verdictDoubts:
		if len(v.Doubts) == 0 {
			return errors.New("verdict 是 doubts，却没列疑点")
		}
	default:
		return fmt.Errorf("verdict 只能是 %s 或 %s，现在是 %q", verdictPass, verdictDoubts, v.Verdict)
	}
	for i, doubt := range v.Doubts {
		if _, ok := doubtLabel(doubt.Kind); !ok {
			return fmt.Errorf("第 %d 条疑点的 kind 不认识：%q", i+1, doubt.Kind)
		}
		if strings.TrimSpace(doubt.Claim) == "" {
			return fmt.Errorf("第 %d 条疑点没写是哪句话", i+1)
		}
	}
	return nil
}

var submitReviewSpec = brain.ToolSpec{Name: submitReviewTool,
	Description: "交回审查结果：草稿里具体的说法都有依据就交 pass；有疑点就交 doubts 并逐条列出。只核对，不改写结论。",
	Parameters:  reviewParameters()}

// reviewParameters 是 submit_review 的参数定义：由 reviewVerdict 生成，结论和问题种类是枚举（交回后 check 照同样的取值核对）。
// 生成不了就是写错了：直接 panic，测试和启动时就暴露。
func reviewParameters() json.RawMessage {
	kinds := make([]any, len(doubtKinds))
	meanings := make([]string, len(doubtKinds))
	for i, known := range doubtKinds {
		kinds[i] = string(known.kind)
		meanings[i] = string(known.kind) + " = " + known.meaning
	}
	schema, err := jsonschema.For[reviewVerdict](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[verdictKind](): {Type: "string", Enum: []any{string(verdictPass), string(verdictDoubts)},
			Description: string(verdictPass) + " = 草稿里具体的说法在摘录里都找得到、意思一致；" + string(verdictDoubts) +
				" = 有疑点（逐条列在 doubts 里）"},
		reflect.TypeFor[doubtKind](): {Type: "string", Enum: kinds, Description: "问题种类：" + strings.Join(meanings, "；")},
	}})
	if err != nil {
		panic(fmt.Sprintf("submit_review 的参数生成不了定义: %v", err))
	}
	data, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("submit_review 的参数定义编不成 JSON: %v", err))
	}
	return data
}

// review 把草稿交给审查（只审一次，有自己的时限）。额度不够、出错、超时、没用 submit_review 交回、交回的不对，都返回错误：
// 调用方用草稿。tokens 是这次审查的用量（日志用）。
func (r *Runner) review(ctx context.Context, question string, draft conclusionDraft, notes sourceNotes,
	now time.Time) (verdict reviewVerdict, tokens int, err error) {
	if err := r.budget.AllowResearch(ctx); err != nil {
		return reviewVerdict{}, 0, err
	}
	material, err := json.Marshal(reviewMaterial{Today: brain.DateText(now), Question: question, Draft: draft.Conclusion,
		Sources: notes.excerptsFor(draft.Sources)})
	if err != nil {
		return reviewVerdict{}, 0, err
	}
	reviewer := r.agents.Reviewer
	reviewCtx, cancel := context.WithTimeout(ctx, reviewer.Timeout)
	defer cancel()
	messages := []brain.ChatMessage{{Role: brain.RoleSystem, Text: reviewer.Instructions}, {Role: brain.RoleUser, Text: string(material)}}
	reply, err := brain.CollectReply(reviewCtx, reviewer.Model, brain.ChatRequest{Messages: messages, Tools: r.reviewSpecs})
	tokens = reply.Usage.PromptTokens + reply.Usage.OutputTokens
	if err != nil {
		if reviewCtx.Err() != nil && ctx.Err() == nil {
			return reviewVerdict{}, tokens, fmt.Errorf("审查超过了 %s", reviewer.Timeout)
		}
		return reviewVerdict{}, tokens, err
	}
	for _, call := range reply.ToolCalls {
		if call.Name != submitReviewTool {
			continue
		}
		verdict, err := brain.DecodeArguments[reviewVerdict](call.Arguments)
		if err == nil {
			err = verdict.check()
		}
		if err != nil {
			return reviewVerdict{}, tokens, fmt.Errorf("%s 的参数不对：%w", submitReviewTool, err)
		}
		return verdict, tokens, nil
	}
	return reviewVerdict{}, tokens, fmt.Errorf("审查没用 %s 交回", submitReviewTool)
}

// reviewOutcome 是审查的结果（只给日志看）。
type reviewOutcome string

const (
	reviewNone          reviewOutcome = "none"           // 没有审查（没配审查的代理），或没走到审查
	reviewPassed        reviewOutcome = "passed"         // 通过：用草稿
	reviewRechecked     reviewOutcome = "rechecked"      // 有疑点，复查后交了新结论
	reviewRecheckFailed reviewOutcome = "recheck_failed" // 有疑点，复查没交出新结论：用草稿、记成部分
	reviewFailed        reviewOutcome = "failed"         // 审查没做成：用草稿
)

// settle 定下交出去的结论：没有审查时就是草稿；有审查时先审（只审一次），通过就用草稿，有疑点就退回复查一次。
// 审查没做成时用草稿，原因里写明没审查成；复查没交出新结论时用草稿、记成「部分」，原因里写明审查发现了疑点、没能复查。
func (q *inquiry) settle(ctx context.Context, draft conclusionDraft, now time.Time) {
	report := &q.report
	report.conclusion, report.sources = draft.Conclusion, draft.Sources
	if q.r.agents.Reviewer == nil {
		return
	}
	verdict, tokens, err := q.r.review(ctx, q.question, draft, q.sources, now)
	report.reviewTokens = tokens
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		q.r.logger.Warn("审查没做成：用草稿", "err", err)
		report.review = reviewFailed
		report.reason = joinReasons(report.reason, "没审查成："+err.Error())
		return
	}
	report.doubts = len(verdict.Doubts)
	if verdict.Verdict == verdictPass {
		report.review = reviewPassed
		return
	}
	revised, err := q.recheck(ctx, verdict.Doubts)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		q.r.logger.Warn("审查发现了疑点，复查没交出新结论：用草稿，记成部分", "doubts", len(verdict.Doubts), "err", err)
		report.review, report.status = reviewRecheckFailed, StatusPartial
		report.reason = joinReasons(report.reason, fmt.Sprintf("审查发现了 %d 处疑点，没能复查：%v", len(verdict.Doubts), err))
		return
	}
	report.review = reviewRechecked
	report.conclusion, report.sources = revised.Conclusion, revised.Sources
}

// draftReceived 是复查前给上一步交草稿的调用回的结果（每个调用后面都要跟着它的结果）。
const draftReceived = "结论收到了。"

// recheckDoubt 是交回给调查的模型的一条疑点。
type recheckDoubt struct {
	Claim    string `json:"claim"`
	Problem  string `json:"problem"`
	Evidence string `json:"evidence"`
}

// doubtsMessage 是把疑点交回给调查的模型的那条消息：疑点照审查交回的原样编成 JSON，问题种类换成说法。
func doubtsMessage(doubts []reviewDoubt) (string, error) {
	items := make([]recheckDoubt, len(doubts))
	for i, doubt := range doubts {
		label, _ := doubtLabel(doubt.Kind) // 种类在 check 里核对过
		items[i] = recheckDoubt{Claim: doubt.Claim, Problem: label, Evidence: doubt.Evidence}
	}
	data, err := json.Marshal(struct {
		Doubts []recheckDoubt `json:"doubts"`
	}{items})
	if err != nil {
		return "", err
	}
	return "核对你交的结论时发现了下面这些疑点。按说明逐条复查，查完再用 " + submitConclusionTool + " 交一次：\n" + string(data), nil
}

// recheck 把审查的疑点交回原来的调查对话复查一次：给同样的工具和一点额外的额度（recheckSearches、recheckRounds、
// Limits.RecheckTimeout；每一轮之前照样查调查的额度），让调查的模型逐条再查、再交一次结论。交出了就返回新结论；
// 没交出（出错、超时、额度用完、轮数用完、交了空的）返回错误。
func (q *inquiry) recheck(ctx context.Context, doubts []reviewDoubt) (conclusionDraft, error) {
	message, err := doubtsMessage(doubts)
	if err != nil {
		return conclusionDraft{}, err
	}
	q.answerCalls(draftReceived)
	q.messages = append(q.messages, brain.ChatMessage{Role: brain.RoleUser, Text: message})
	recheckCtx, cancel := context.WithTimeout(ctx, q.r.limits.RecheckTimeout)
	defer cancel()
	searchCap := q.report.searches + recheckSearches
	for range recheckRounds {
		if err := q.r.budget.AllowResearch(ctx); err != nil {
			return conclusionDraft{}, err
		}
		q.report.recheckRounds++
		draft, concluded, err := q.step(recheckCtx, q.r.investigationSpecs, searchCap)
		if err != nil {
			if recheckCtx.Err() != nil && ctx.Err() == nil {
				return conclusionDraft{}, fmt.Errorf("复查超过了 %s", q.r.limits.RecheckTimeout)
			}
			return conclusionDraft{}, err
		}
		if !concluded {
			continue
		}
		if draft.Conclusion == "" {
			return conclusionDraft{}, errors.New("复查交的结论是空的")
		}
		return draft, nil
	}
	return conclusionDraft{}, fmt.Errorf("复查了 %d 轮还没交新结论", recheckRounds)
}

// joinReasons 把几段原因接起来（空的跳过）。
func joinReasons(reasons ...string) string {
	var kept []string
	for _, reason := range reasons {
		if reason != "" {
			kept = append(kept, reason)
		}
	}
	return strings.Join(kept, "；")
}
