package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

// 确认闸：标注为「会破坏」的工具（ToolSpec.RequiresConfirmation）不直接执行。
//  1. 模型调用它 → 先核对这一轮说话的是登记过声纹的人；不是就不做。
//  2. 是 → 存成一条待确认的操作（编号、工具、参数、到期时间），把要点交给模型，由模型用自己的话问主人。
//  3. 主人回答。这句话照常交给模型；这一轮多两个会话工具：confirm_action、cancel_action。主人算不算同意，由模型判断。
//  4. confirm_action → 再核对：编号对得上、没过期、是之后的一轮、这一轮说话的是登记过的人 → 用当初存下的参数执行
//     （不用模型这次给的）。cancel_action → 作废，同一个人其他待确认的一并作废。过期的自动作废。
// 只有「这一次允许」和「拒绝」，没有「以后都允许」。

const (
	confirmActionTool = "confirm_action"
	cancelActionTool  = "cancel_action"
)

// DefaultConfirmationTimeout 是待确认的操作多久没确认就作废。
const DefaultConfirmationTimeout = 60 * time.Second

// pendingAction 是一条待确认的操作。
type pendingAction struct {
	ID        string
	Call      ToolCall  // 当初模型要求的调用（执行时用这里的参数）
	Turn      int64     // 在第几轮发起的（确认必须在之后的一轮）
	ExpiresAt time.Time //
}

// turnCounter 给每一轮编号（确认必须在发起之后的一轮）。
var turnCounter atomic.Int64

type actionInput struct {
	ActionID string `json:"action_id" jsonschema:"待确认操作的编号（needs_confirmation 结果里的 action_id）"`
}

var (
	confirmActionSpec = ToolSpec{Name: confirmActionTool, Parameters: ParametersFor[actionInput](),
		Description: "主人同意了刚才问的那件事时调用：按编号执行那次待确认的操作（用当初的参数）。" +
			"主人的回答算不算同意由你判断：明确同意才调用；没听清、答非所问、只是叫了你，都不算同意，可以再问一次。"}
	cancelActionSpec = ToolSpec{Name: cancelActionTool, Parameters: ParametersFor[actionInput](),
		Description: "主人不同意、或者说算了时调用：作废那次待确认的操作（同一个人别的待确认操作也一并作废）。"}
)

// confirmationResult 是交给模型的确认闸结果。
type confirmationResult struct {
	Status    string         `json:"status"` // needs_confirmation / denied / refused / cancelled / done
	ActionID  string         `json:"action_id,omitempty"`
	Tool      string         `json:"tool,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	ExpiresIn int            `json:"expires_in_seconds,omitempty"`
	Result    string         `json:"result,omitempty"` // 执行之后工具交回的结果
	Note      string         `json:"note,omitempty"`
}

func (r confirmationResult) text() string {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Sprintf(`{"status":"%s"}`, r.Status)
	}
	return string(data)
}

// requiresConfirmation 判断这个工具是不是要过确认闸。
func (s *Session) requiresConfirmation(name string) bool {
	if name == raiseMonthlyCapTool {
		return raiseMonthlyCapSpec.RequiresConfirmation
	}
	if s.parts.Tools == nil {
		return false
	}
	for _, spec := range s.parts.Tools.Specs() {
		if spec.Name == name {
			return spec.RequiresConfirmation
		}
	}
	return false
}

// holdForConfirmation 拦下一次会破坏的调用：说话的不是登记过的人就不做；是就存成待确认的操作，把要点交给模型。
func (s *Session) holdForConfirmation(record *turnRecord, call ToolCall) string {
	if !record.voiceVerified {
		s.logger.Info("要确认的操作：这句话没认出是登记过的人，不做", "tool", call.Name)
		return confirmationResult{Status: "denied", Tool: call.Name,
			Note: "这件事只有登记过声纹的人能让你做，这句话没认出是谁，所以没有做。照实告诉对方。"}.text()
	}
	timeout := s.opts.ConfirmationTimeout
	if timeout <= 0 {
		timeout = DefaultConfirmationTimeout
	}
	action := pendingAction{ID: s.currentConversation().nextActionID(), Call: call, Turn: record.turn,
		ExpiresAt: time.Now().Add(timeout)}
	s.currentConversation().addPendingAction(action)
	record.askedConfirmation = true
	s.logger.Info("要确认的操作：先问主人", "tool", call.Name, "action", action.ID, "expires_in", timeout)
	return confirmationResult{Status: "needs_confirmation", ActionID: action.ID, Tool: call.Name, Arguments: call.Arguments,
		ExpiresIn: int(timeout.Seconds()),
		Note: "还没有执行。用你自己的话把要做的事（按上面的参数）说清楚，问主人要不要做；" +
			"主人回答后，同意就调用 confirm_action，不同意就调用 cancel_action。"}.text()
}

// confirmAction 执行 confirm_action：四项核对都过了，才用当初存下的参数执行。
func (s *Session) confirmAction(ctx context.Context, record *turnRecord, call ToolCall) string {
	in, err := DecodeArguments[actionInput](call.Arguments)
	if err != nil {
		return confirmationResult{Status: "refused", Note: err.Error()}.text()
	}
	conversation := s.currentConversation()
	action, found := conversation.pendingAction(in.ActionID)
	refuse := func(note string) string {
		s.logger.Info("确认没通过", "action", in.ActionID, "why", note)
		return confirmationResult{Status: "refused", ActionID: in.ActionID, Note: note}.text()
	}
	switch {
	case !found:
		return refuse("没有这个编号的待确认操作（可能已经过期、被取消或已经执行过）：没有执行。要做的话重新调用那个工具。")
	case time.Now().After(action.ExpiresAt):
		conversation.takePendingAction(in.ActionID)
		return refuse("已经过期了，没有执行。告诉主人已经过期；要做的话重新调用那个工具，再问一次。")
	case action.Turn >= record.turn:
		return refuse("要等主人回答之后（下一句话）才能确认：先问主人，不要自己确认。")
	case !record.voiceVerified:
		return refuse("这句话没认出是登记过声纹的人，不能确认：没有执行。")
	}
	conversation.takePendingAction(in.ActionID)
	s.logger.Info("主人确认了：执行", "action", action.ID, "tool", action.Call.Name)
	return confirmationResult{Status: "done", ActionID: action.ID, Tool: action.Call.Name,
		Result: s.executeConfirmed(ctx, record, action.Call)}.text()
}

// cancelAction 执行 cancel_action：作废这个人所有待确认的操作。
func (s *Session) cancelAction(call ToolCall) string {
	in, _ := DecodeArguments[actionInput](call.Arguments) // 编号不对也照样作废全部：主人说了不要
	dropped := s.currentConversation().dropPendingActions()
	s.logger.Info("主人不同意：作废待确认的操作", "action", in.ActionID, "dropped", dropped)
	return confirmationResult{Status: "cancelled", ActionID: in.ActionID, Note: "已作废，没有执行。"}.text()
}

// confirmationSpecs 是这一轮要不要多给 confirm_action、cancel_action：有没过期的待确认操作时才给。
func (s *Session) confirmationSpecs() []ToolSpec {
	if !s.currentConversation().hasPendingActions(time.Now()) {
		return nil
	}
	return []ToolSpec{confirmActionSpec, cancelActionSpec}
}
