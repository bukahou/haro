package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/bukahou/haro/internal/money"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 月度费用到了上限时：普通的问题不回答，念固定的话（到上限了、下个月一号恢复、可以口头临时提高）；
// 主人要临时提高时过确认闸（登记过的声纹 + 有时效的确认），确认后才提高。
// 听懂「要提高」只能靠模型（不靠词表）：上限到了以后每句话交给代理「额度判断」（internal/agents/builtin/over_budget.md）
// 用一次很小的调用判断 —— 不带历史，只给「提高上限」和确认的两个工具；模型说的话不念，念给主人的都是固定的话。

// RaiseMonthlyCapTool 是「提高月度上限」的工具名（额度那一层核对主人的确认时也按它，见 RequireConfirmation）。
const RaiseMonthlyCapTool = "raise_monthly_cap"

// ErrMonthlyCapReached 表示这个月的费用到了上限（errors.Is 判断：额度那一层交出的错误要能认出它）。
var ErrMonthlyCapReached = errors.New("这个月的费用到了上限")

// MonthlyCap 是月度费用上限：主人口头确认后可以临时提高（额度那一层实现）。
type MonthlyCap interface {
	// RaiseMonthlyCap 把这个月的上限提高一次（加多少由配置定），返回提高后的上限（默认货币）。
	// ctx 里要带着 person 对 RaiseMonthlyCapTool 的确认（WithConfirmation），没有就拒绝。
	RaiseMonthlyCap(ctx context.Context, person string) (money.Amount, error)
}

var raiseMonthlyCapSpec = ToolSpec{Name: RaiseMonthlyCapTool, Parameters: ParametersFor[struct{}](), RequiresConfirmation: true,
	Description: "主人明确要临时提高这个月的费用上限时调用（每次提高的金额是固定的）。会先问主人确认，主人同意后才提高。"}

// OverBudgetPromptValues 是代理「额度判断」正文里引用的值：工具名只在代码里定义一处。只填它的允许名单里有的工具
// （allowed 为 nil = 全部）：正文提到没交给它的工具时填值报错、服务端起不来，不会让模型去调用一个没给它的工具。
func OverBudgetPromptValues(allowed []string) map[string]any {
	values := map[string]any{}
	for key, tool := range map[string]string{"RaiseTool": RaiseMonthlyCapTool, "ConfirmTool": confirmActionTool, "CancelTool": cancelActionTool} {
		if allowed == nil || slices.Contains(allowed, tool) {
			values[key] = tool
		}
	}
	return values
}

// canRaiseMonthlyCap 是这个服务端能不能口头提高上限（接了上限、也有判断用的代理）。
func (s *Session) canRaiseMonthlyCap() bool {
	return s.parts.MonthlyCap != nil && s.parts.OverBudget != nil
}

// capRaiseResult 是「提高上限」交给模型的结果。
type capRaiseResult struct {
	Raised       string `json:"raised"`        // 例如「500 JPY」
	MonthlyLimit string `json:"monthly_limit"` // 提高后的上限
}

// raiseMonthlyCap 执行主人确认过的「提高上限」（ctx 里带着主人的确认）。
func (s *Session) raiseMonthlyCap(ctx context.Context, record *turnRecord) (string, error) {
	limit, err := s.parts.MonthlyCap.RaiseMonthlyCap(ctx, s.currentConversation().Person())
	if err != nil {
		s.logger.Warn("💴 提高这个月的上限失败", "err", err)
		return "", err
	}
	record.capRaisedTo = limit
	s.logger.Info("💴 主人确认：这个月的上限提高了", "raised", s.opts.MonthlyRaise.String(), "limit", limit.String())
	data, err := json.Marshal(capRaiseResult{Raised: s.opts.MonthlyRaise.String(), MonthlyLimit: limit.String()})
	return string(data), err
}

// executeConfirmed 执行主人确认过的操作（ctx 里带着主人的确认）：会话自己的工具在这里做，别的交给工具箱。
func (s *Session) executeConfirmed(ctx context.Context, record *turnRecord, call ToolCall) (string, error) {
	if call.Name == RaiseMonthlyCapTool && s.parts.MonthlyCap != nil {
		return s.raiseMonthlyCap(ctx, record)
	}
	return s.runTool(ctx, record, call)
}

// answerOverBudget 上限到了时回答这一句（见文件开头）。capErr 是额度那一层交出的错误（能念给主人）。
func (s *Session) answerOverBudget(ctx context.Context, record *turnRecord, capErr error) realtimeproto.ResponseDone {
	judge := s.parts.OverBudget
	messages := []ChatMessage{{Role: RoleSystem, Text: judge.Instructions}}
	if notes := s.pendingActionNotes(); len(notes) > 0 {
		messages = append(messages, ChatMessage{Role: RoleUser, Text: situationHeading + joinLines(notes), Synthetic: true})
	}
	messages = append(messages, ChatMessage{Role: RoleUser, Text: record.question})
	tools := judge.offer(append([]ToolSpec{raiseMonthlyCapSpec}, s.confirmationSpecs()...))
	reply, err := CollectReply(ctx, judge.Model, ChatRequest{Messages: messages, Tools: tools})
	if ctx.Err() != nil {
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}
	}
	if err != nil { // 判断失败（每天的次数用完、连不上……）：照样告诉主人到上限了
		s.logger.Warn("💴 上限到了：判断要不要提高的那次调用没成", "err", err)
	}
	for _, call := range reply.ToolCalls {
		if !slices.ContainsFunc(tools, func(spec ToolSpec) bool { return spec.Name == call.Name }) { // 执行方自己守门：只做给了它的
			s.logger.Warn("💴 额度判断调用了没给它的工具：不执行", "tool", call.Name)
			continue
		}
		switch call.Name {
		case RaiseMonthlyCapTool:
			s.holdForConfirmation(ctx, record, call)
		case confirmActionTool:
			s.confirmAction(ctx, record, call)
		case cancelActionTool:
			s.cancelAction(call)
		}
	}
	s.mu.Lock()
	language := s.language
	s.mu.Unlock()
	switch {
	case record.capRaisedTo.Micros > 0:
		return s.speakFixedReply(ctx, s.opts.Phrases.Say(language, PhraseMonthlyCapRaised,
			map[string]string{"limit": record.capRaisedTo.Rounded(), PhraseCurrency: record.capRaisedTo.Currency}))
	case record.askedConfirmation:
		return s.speakFixedReply(ctx, s.opts.Phrases.Say(language, PhraseConfirmCapRaise,
			map[string]string{"raise": s.opts.MonthlyRaise.Rounded(), PhraseCurrency: s.opts.MonthlyRaise.Currency}))
	}
	var spoken SpokenError
	if errors.As(capErr, &spoken) {
		key, values := spoken.Phrase()
		return s.speakFixedReply(ctx, s.opts.Phrases.Say(language, key, values))
	}
	return s.failed(realtimeproto.ReasonGenerationFailed, "生成回答", capErr)
}

// pendingActionNotes 是还没过期的待确认操作（那次小调用不带历史，靠这几行知道刚才问过什么）。
func (s *Session) pendingActionNotes() []string {
	var notes []string
	for _, action := range s.currentConversation().unexpiredPendingActions(time.Now()) {
		what := action.Call.Name
		if action.Call.Name == RaiseMonthlyCapTool {
			what = "把这个月的费用上限临时提高 " + s.opts.MonthlyRaise.String()
		}
		notes = append(notes, fmt.Sprintf("待确认的操作 %s：%s（刚才已经问过主人要不要做）", action.ID, what))
	}
	return notes
}
