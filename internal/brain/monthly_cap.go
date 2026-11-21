package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// 月度费用到了上限时：普通的问题不回答，念固定的话（到上限了、下个月一号恢复、可以口头临时提高）；
// 主人要临时提高时过确认闸（登记过的声纹 + 有时效的确认），确认后才提高。
// 听懂「要提高」只能靠模型（不靠词表）：上限到了以后每句话用一次很小的调用判断 —— 不带历史，
// 只给「提高上限」和确认的两个工具；模型说的话不念，念给主人的都是固定的话。

const raiseMonthlyCapTool = "raise_monthly_cap"

// ErrMonthlyCapReached 表示这个月的费用到了上限（errors.Is 判断：额度那一层交出的错误要能认出它）。
var ErrMonthlyCapReached = errors.New("这个月的费用到了上限")

// MonthlyCap 是月度费用上限：主人口头确认后可以临时提高（额度那一层实现）。
type MonthlyCap interface {
	// RaiseMonthlyCap 把这个月的上限提高一次（加多少由配置定），返回提高后的上限（日元）。
	RaiseMonthlyCap(ctx context.Context, person string) (float64, error)
}

var raiseMonthlyCapSpec = ToolSpec{Name: raiseMonthlyCapTool, Parameters: ParametersFor[struct{}](), RequiresConfirmation: true,
	Description: "主人明确要临时提高这个月的费用上限时调用（每次提高的金额是固定的）。会先问主人确认，主人同意后才提高。"}

// overBudgetInstructions 是上限到了以后那次小调用的系统提示词：只判断要不要提高、同不同意，不回答问题。
const overBudgetInstructions = `这个月的费用到了上限，现在不回答任何问题，也不要输出文字。只判断主人这句话：
- 要临时提高这个月的费用上限：调用 raise_monthly_cap；
- 情况说明里列着待确认的操作：主人同意就调用 confirm_action，不同意就调用 cancel_action；
- 其余情况：什么都不调用。`

// canRaiseMonthlyCap 是这个服务端能不能口头提高上限（接了上限、也有判断用的模型）。
func (s *Session) canRaiseMonthlyCap() bool {
	return s.parts.MonthlyCap != nil && s.parts.OverBudgetModel != nil
}

// capRaiseResult 是「提高上限」交给模型的结果。
type capRaiseResult struct {
	RaisedYen       float64 `json:"raised_yen,omitempty"`
	MonthlyLimitYen float64 `json:"monthly_limit_yen,omitempty"`
	Error           string  `json:"error,omitempty"`
}

// raiseMonthlyCap 执行主人确认过的「提高上限」。
func (s *Session) raiseMonthlyCap(ctx context.Context, record *turnRecord) string {
	result := capRaiseResult{RaisedYen: s.opts.MonthlyRaiseYen}
	limit, err := s.parts.MonthlyCap.RaiseMonthlyCap(ctx, s.currentConversation().Person())
	if err != nil {
		s.logger.Warn("💴 提高这个月的上限失败", "err", err)
		result = capRaiseResult{Error: err.Error()}
	} else {
		record.capRaisedTo = limit
		result.MonthlyLimitYen = limit
		s.logger.Info("💴 主人确认：这个月的上限提高了", "raised_yen", s.opts.MonthlyRaiseYen, "limit_yen", limit)
	}
	data, _ := json.Marshal(result)
	return string(data)
}

// executeConfirmed 执行主人确认过的操作：会话自己的工具在这里做，别的交给工具箱。
func (s *Session) executeConfirmed(ctx context.Context, record *turnRecord, call ToolCall) string {
	if call.Name == raiseMonthlyCapTool && s.parts.MonthlyCap != nil {
		return s.raiseMonthlyCap(ctx, record)
	}
	return s.callTool(ctx, record, call)
}

// answerOverBudget 上限到了时回答这一句（见文件开头）。capErr 是额度那一层交出的错误（能念给主人）。
func (s *Session) answerOverBudget(ctx context.Context, record *turnRecord, capErr error) realtimeproto.ResponseDone {
	messages := []ChatMessage{{Role: RoleSystem, Text: overBudgetInstructions}}
	if notes := s.pendingActionNotes(); len(notes) > 0 {
		messages = append(messages, ChatMessage{Role: RoleUser, Text: situationHeading + joinLines(notes), Synthetic: true})
	}
	messages = append(messages, ChatMessage{Role: RoleUser, Text: record.question})
	tools := append([]ToolSpec{raiseMonthlyCapSpec}, s.confirmationSpecs()...)
	reply, err := CollectReply(ctx, s.parts.OverBudgetModel, ChatRequest{Messages: messages, Tools: tools})
	if ctx.Err() != nil {
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}
	}
	if err != nil { // 判断失败（每天的次数用完、连不上……）：照样告诉主人到上限了
		s.logger.Warn("💴 上限到了：判断要不要提高的那次调用没成", "err", err)
	}
	for _, call := range reply.ToolCalls {
		switch call.Name {
		case raiseMonthlyCapTool:
			s.holdForConfirmation(record, call)
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
	case record.capRaisedTo > 0:
		return s.speakFixedReply(ctx, s.opts.Phrases.Say(language, PhraseMonthlyCapRaised,
			map[string]string{"limit": strconv.FormatFloat(record.capRaisedTo, 'f', 0, 64)}))
	case record.askedConfirmation:
		return s.speakFixedReply(ctx, s.opts.Phrases.Say(language, PhraseConfirmCapRaise,
			map[string]string{"raise": strconv.FormatFloat(s.opts.MonthlyRaiseYen, 'f', 0, 64)}))
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
		if action.Call.Name == raiseMonthlyCapTool {
			what = fmt.Sprintf("把这个月的费用上限临时提高 %.0f 日元", s.opts.MonthlyRaiseYen)
		}
		notes = append(notes, fmt.Sprintf("待确认的操作 %s：%s（刚才已经问过主人要不要做）", action.ID, what))
	}
	return notes
}
