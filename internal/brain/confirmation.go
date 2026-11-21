package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// 确认闸：标注为「会破坏」的工具（ToolSpec.RequiresConfirmation）不直接执行。
//  1. 模型调用它 → 先核对这一轮说话的是登记过声纹的人；不是就不做。再按工具的定义校验参数；不对就把哪里不对交回模型，不存下。
//  2. 都对 → 存成一条待确认的操作（编号、工具、参数、到期时间），把要点交给模型，由模型用自己的话问主人。
//  3. 主人回答。这句话照常交给模型；主人算不算同意由模型判断，同意就调用 confirm_action、不同意就调用 cancel_action
//     （有会破坏的工具时这两个工具的定义一直带着，见 sessionTool.gate）。
//  4. confirm_action → 再核对：编号对得上、没过期、是之后的一轮、这一轮说话的是登记过的人 → 用当初存下的参数执行
//     （不用模型这次给的），执行时带上主人的确认（Confirmation，经 ctx）；执行失败时交回 failed，不算做成。
//     cancel_action → 作废，同一个人其他待确认的一并作废。过期的自动作废。
// 只有「这一次允许」和「拒绝」，没有「以后都允许」。
//
// 执行方自己也守门（不假设上游拦过）：工具箱里标了会破坏的工具、提高月度上限都用 RequireConfirmation 核对，
// 见不到主人对这次操作的确认就拒绝。以后别的入口（手机、后台代理）要执行会破坏的操作，也要先取得主人的确认、带着它来。

// Confirmation 是主人对一次会破坏的操作的确认，由确认闸在执行时放进 ctx（WithConfirmation），执行方据此核对。
type Confirmation struct {
	ActionID string // 待确认操作的编号
	Tool     string // 确认的是哪个工具
	Person   string // 谁确认的（按声纹认出的对话名）
}

type confirmationKey struct{}

// WithConfirmation 把主人的确认放进执行用的 ctx。
func WithConfirmation(ctx context.Context, confirmation Confirmation) context.Context {
	return context.WithValue(ctx, confirmationKey{}, confirmation)
}

// ConfirmationFrom 取出 ctx 里主人的确认；没有时 ok 为 false。
func ConfirmationFrom(ctx context.Context) (Confirmation, bool) {
	confirmation, ok := ctx.Value(confirmationKey{}).(Confirmation)
	return confirmation, ok
}

// ErrNotConfirmed 表示执行方没见到主人对这次操作的确认（errors.Is 判断）。
var ErrNotConfirmed = errors.New("没有主人的确认")

// RequireConfirmation 是执行方的核对：ctx 里要带着主人对 tool 这次操作的确认，确认的人是按声纹认出的人；
// person 不为空时还要是这个人确认的。
func RequireConfirmation(ctx context.Context, tool, person string) (Confirmation, error) {
	confirmation, ok := ConfirmationFrom(ctx)
	switch {
	case !ok:
		return Confirmation{}, fmt.Errorf("%w：%s 会删掉或改掉东西，要主人确认后才能执行", ErrNotConfirmed, tool)
	case confirmation.Tool != tool:
		return Confirmation{}, fmt.Errorf("%w：主人确认的是 %s，不是 %s", ErrNotConfirmed, confirmation.Tool, tool)
	case confirmation.Person == "" || confirmation.Person == SharedPerson:
		return Confirmation{}, fmt.Errorf("%w：%s 的确认没有认出是谁", ErrNotConfirmed, tool)
	case person != "" && confirmation.Person != person:
		return Confirmation{}, fmt.Errorf("%w：%s 是 %s 确认的，不是 %s", ErrNotConfirmed, tool, confirmation.Person, person)
	}
	return confirmation, nil
}

const (
	confirmActionTool = "confirm_action"
	cancelActionTool  = "cancel_action"
)

// DefaultConfirmationTimeout 是待确认的操作多久没确认就作废：Options.ConfirmationTimeout 为零时用（服务端没设
// HARO_CONFIRMATION_TIMEOUT 时就是它，默认值只写在这里）。
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
	Status    string          `json:"status"` // needs_confirmation / invalid_arguments / denied / refused / cancelled / done / failed
	ActionID  string          `json:"action_id,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"` // 存下的参数（校验时编好的 JSON）
	ExpiresIn int             `json:"expires_in_seconds,omitempty"`
	Result    string          `json:"result,omitempty"` // 执行之后工具交回的结果；失败时是出错的说明
	Note      string          `json:"note,omitempty"`
}

// text 编成交给模型的 JSON：字段都是字符串、数字和已经编好的 JSON（Arguments），编码不会出错。
func (r confirmationResult) text() string {
	data, _ := json.Marshal(r)
	return string(data)
}

// specOf 是名为 name 的工具的定义：会话自己的（这一轮不提供时也给出它的定义）、工具箱里的；不认识时 ok 为 false。
func (s *Session) specOf(name string) (ToolSpec, bool) {
	for _, tool := range sessionTools {
		if tool.name == name {
			spec, _ := tool.offer(s)
			return spec, true
		}
	}
	if s.parts.Tools != nil {
		for _, spec := range s.parts.Tools.Specs() {
			if spec.Name == name {
				return spec, true
			}
		}
	}
	return ToolSpec{}, false
}

// requiresConfirmation 判断这个工具是不是要过确认闸（看它的定义上的标注）。
func (s *Session) requiresConfirmation(name string) bool {
	spec, ok := s.specOf(name)
	return ok && spec.RequiresConfirmation
}

// validateArguments 按工具的参数定义（JSON Schema）校验模型给的参数，交回编成 JSON 的参数。和执行时那一层同一个定义：
// 问主人之前就知道参数对不对，不让主人确认一个执行不了的操作。
func validateArguments(parameters json.RawMessage, arguments map[string]any) (json.RawMessage, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("参数格式不对: %w", err)
	}
	if len(parameters) == 0 {
		return raw, nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(parameters, &schema); err != nil {
		return nil, fmt.Errorf("工具的参数定义读不出: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("工具的参数定义: %w", err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("参数格式不对: %w", err)
	}
	if err := resolved.Validate(value); err != nil {
		return nil, fmt.Errorf("参数不对: %w", err)
	}
	return raw, nil
}

// holdForConfirmation 拦下一次会破坏的调用：说话的不是登记过的人就不做；参数不对就交回哪里不对、不存下；
// 都对就存成待确认的操作，把要点交给模型。
func (s *Session) holdForConfirmation(record *turnRecord, call ToolCall) string {
	if !record.voiceVerified {
		s.logger.Info("要确认的操作：这句话没认出是登记过的人，不做", "tool", call.Name)
		return confirmationResult{Status: "denied", Tool: call.Name,
			Note: "这件事只有登记过声纹的人能让你做，这句话没认出是谁，所以没有做。照实告诉对方。"}.text()
	}
	spec, _ := s.specOf(call.Name)
	arguments, err := validateArguments(spec.Parameters, call.Arguments)
	if err != nil {
		s.logger.Info("要确认的操作：参数不对，没有存下", "tool", call.Name, "err", err)
		return confirmationResult{Status: "invalid_arguments", Tool: call.Name,
			Note: err.Error() + "。没有存下，也还没问主人：按工具的参数定义改好再调用。"}.text()
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
	return confirmationResult{Status: "needs_confirmation", ActionID: action.ID, Tool: call.Name, Arguments: arguments,
		ExpiresIn: int(timeout.Seconds()),
		Note: "还没有执行。用你自己的话把要做的事（按上面的参数）说清楚，问主人要不要做；" +
			"主人回答后，同意就调用 " + confirmActionTool + "，不同意就调用 " + cancelActionTool + "。"}.text()
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
	confirmed := WithConfirmation(ctx, Confirmation{ActionID: action.ID, Tool: action.Call.Name, Person: conversation.Person()})
	result, err := s.executeConfirmed(confirmed, record, action.Call)
	if err != nil {
		s.logger.Warn("主人确认了，但执行失败", "action", action.ID, "tool", action.Call.Name, "err", err)
		return confirmationResult{Status: "failed", ActionID: action.ID, Tool: action.Call.Name, Result: err.Error(),
			Note: "主人确认了，但没有做成（原因见 result）。照实告诉主人没做成，不要说做好了；要再做的话重新调用那个工具，再问一次。"}.text()
	}
	return confirmationResult{Status: "done", ActionID: action.ID, Tool: action.Call.Name, Result: result}.text()
}

// cancelAction 执行 cancel_action：作废这个人所有待确认的操作。
func (s *Session) cancelAction(call ToolCall) string {
	in, _ := DecodeArguments[actionInput](call.Arguments) // 编号不对也照样作废全部：主人说了不要
	dropped := s.currentConversation().dropPendingActions()
	s.logger.Info("主人不同意：作废待确认的操作", "action", in.ActionID, "dropped", dropped)
	return confirmationResult{Status: "cancelled", ActionID: in.ActionID, Note: "已作废，没有执行。"}.text()
}

// confirmationSpecs 是确认、取消两个工具的定义：有会破坏的工具时一直有（见 sessionTool.gate）。
func (s *Session) confirmationSpecs() []ToolSpec { return s.sessionToolSpecs(true) }
