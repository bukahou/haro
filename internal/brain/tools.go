package brain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"

	"haro/internal/realtimeproto"
)

const (
	maxToolRounds = 3                // 一句话最多几轮工具调用（防止小模型陷入循环）
	toolTimeout   = 10 * time.Second // 单次工具执行的超时
)

// lookAtCameraTool 是大脑内置的「看画面」工具（D-B ②）：画面由终端每轮提供，模型需要看时才交给它。
const lookAtCameraTool = "look_at_camera"

var lookAtCameraSpec = ToolSpec{Name: lookAtCameraTool,
	Description: "看摄像头拍到的当前画面。只有主人问到看得见的东西时才用（例如：你能看到我吗、我旁边有什么、我手里拿着什么、我穿的什么）。",
	Parameters:  json.RawMessage(`{"type":"object","properties":{}}`)}

// cameraFrameNote 是承载画面的那条消息的文字；frameRecalledNote 是它写进历史时的替身（历史里不存图片）。
const (
	cameraFrameNote   = "这是摄像头当前画面。"
	frameRecalledNote = "（当时看了摄像头画面；画面不保留，现在要看需要重新调用 look_at_camera）"
)

// toolSpecs 返回可用的工具清单：内置的看画面 + 工具箱（内置时间 + MCP）。
func (s *Session) toolSpecs() []ToolSpec {
	var specs []ToolSpec
	s.mu.Lock()
	noCamera := s.noCamera || s.cameraGone
	s.mu.Unlock()
	if !noCamera {
		specs = append(specs, lookAtCameraSpec)
	}
	if spec, ok := switchPersonaSpec(s.opts.Personas); ok {
		specs = append(specs, spec)
	}
	if s.parts.Tools != nil {
		specs = append(specs, s.parts.Tools.Specs()...)
	}
	return specs
}

// lookAtCamera 返回交给模型的工具结果，以及承载画面的用户消息（没有画面时为空）。
func (s *Session) lookAtCamera(frame []byte) (string, []ChatMessage) {
	if len(frame) == 0 {
		s.logger.Info("调用工具", "tool", lookAtCameraTool, "frame", false)
		return "摄像头现在没有画面，看不到。", nil
	}
	s.logger.Info("调用工具", "tool", lookAtCameraTool, "frame_bytes", len(frame))
	return "已拿到摄像头当前画面，见下一条消息。", []ChatMessage{{Role: RoleUser, Text: cameraFrameNote, Image: frame}}
}

// needsConfirmation 判断这批调用里有没有要用户口头确认的（MCP destructiveHint，T3 ①）。
func (s *Session) needsConfirmation(calls []ToolCall) bool {
	for _, spec := range s.toolSpecs() {
		for _, call := range calls {
			if call.Name == spec.Name && spec.RequiresConfirmation {
				return true
			}
		}
	}
	return false
}

// callTool 执行一次工具调用，返回交给模型的结果文字；出错时把错误也交给模型，让它如实告诉用户。
func (s *Session) callTool(ctx context.Context, call ToolCall) string {
	ctx, span := tracer.Start(ctx, "brain.tool")
	defer span.End()
	ctx, cancel := context.WithTimeout(WithSpeaker(ctx, s.currentConversation().Person()), toolTimeout)
	defer cancel()
	started := time.Now()
	result, err := s.parts.Tools.Call(ctx, call)
	span.SetAttributes(attribute.String("haro.tool.name", call.Name))
	recordSpanError(span, err)
	arguments, _ := json.Marshal(call.Arguments)
	s.logger.Info("调用工具", "tool", call.Name, "arguments", string(arguments), "elapsed", since(started), "err", err)
	if err != nil {
		var spoken SpokenError
		if errors.As(err, &spoken) { // 例如额度用完：不是暂时的，不提示重试
			return "工具执行失败：" + err.Error()
		}
		// 实测（2026-11-17）：只给错误时，小模型之后主人再问也不再调用，一直说「服务在打瞌睡」
		return "工具执行失败：" + err.Error() + "（多半是暂时的：如实告诉主人这次没查到；主人再问时要重新调用这个工具，不要凭之前的失败回答）"
	}
	return result
}

// assignToolCallIDs 给没有 ID 的调用补上会话内唯一的 ID（ollama 不一定给 ID）。
func (s *Session) assignToolCallIDs(calls []ToolCall) {
	conversation := s.currentConversation()
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = conversation.nextToolCallID()
		}
	}
}

// confirmationQuestion 是执行需要确认的操作之前问用户的话；删除时把要删的内容描述说出来。
func confirmationQuestion(calls []ToolCall) string {
	for _, call := range calls {
		if !strings.HasPrefix(call.Name, "delete") {
			return "确定要这么做吗？说「确认」我就执行。"
		}
	}
	var targets []string
	for _, call := range calls {
		targets = append(targets, deleteTargets(call.Arguments)...)
	}
	switch len(targets) {
	case 0:
		return "确定要删掉吗？说「确认」我就删。"
	case 1:
		return "确定要删掉和「" + targets[0] + "」有关的那条吗？说「确认」我就删。"
	default:
		return "确定要删掉和「" + strings.Join(targets, "」「") + "」有关的这几条吗？说「确认」我就删。"
	}
}

// deleteTargets 取出删除工具参数里的描述：queries（v3 D-1，一次删多条）或旧的 query。
func deleteTargets(arguments map[string]any) []string {
	var targets []string
	add := func(value any) {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			targets = append(targets, strings.TrimSpace(text))
		}
	}
	if list, ok := arguments["queries"].([]any); ok {
		for _, item := range list {
			add(item)
		}
	}
	add(arguments["query"])
	return targets
}

// confirmationAnswer 是用户对确认问题的回答类型。
type confirmationAnswer int

const (
	confirmOther confirmationAnswer = iota // 说了别的：放弃待确认的操作，照常处理这句话
	confirmYes
	confirmNo
)

// classifyConfirmation 判断用户这句是不是在回答确认问题（只看短句，长句一律算「说了别的」）。
func classifyConfirmation(text string) confirmationAnswer {
	trimmed := strings.TrimFunc(text, func(r rune) bool { return strings.ContainsRune("，,。.!！?？、~～… ", r) })
	if trimmed == "" || utf8.RuneCountInString(trimmed) > 8 {
		return confirmOther
	}
	for _, word := range []string{"不", "别", "算了", "取消"} { // 先看否定：「不确定」里也有「确定」
		if strings.Contains(trimmed, word) {
			return confirmNo
		}
	}
	for _, word := range []string{"确认", "确定", "删吧", "删掉", "删了", "是的", "对", "好", "可以", "执行", "嗯"} {
		if strings.Contains(trimmed, word) {
			return confirmYes
		}
	}
	return confirmOther
}

// takePendingConfirmation 取走待确认的操作（只保留一轮：取走之后就没了）。
func (s *Session) takePendingConfirmation() []ToolCall {
	return s.currentConversation().takePendingConfirmation()
}

func (s *Session) setPendingConfirmation(calls []ToolCall) {
	s.currentConversation().setPendingConfirmation(calls)
}

// handleConfirmation 处理上一轮留下的待确认操作；返回 handled=false 表示这句话要照常交给模型。
func (s *Session) handleConfirmation(ctx context.Context, text string, pending []ToolCall) (realtimeproto.ResponseDone, bool) {
	switch classifyConfirmation(text) {
	case confirmYes:
		trail := []ChatMessage{{Role: RoleAssistant, ToolCalls: pending}} // 真正执行的操作也记进历史
		for _, call := range pending {
			trail = append(trail, ChatMessage{Role: RoleTool, ToolName: call.Name, ToolCallID: call.ID,
				Text: s.callTool(ctx, call)})
		}
		// 由模型根据执行结果回答（v3 D-1）：删了哪几条、哪些没找到、失败了为什么，固定的「删掉了」说不清这些。
		history := append(s.historyWithInstructions(), s.userTurn(text))
		history = append(history, trail[:len(trail)-1]...)
		answer, moreTrail, usage, err := s.speakAnswer(ctx, history, trail[len(trail)-1], nil)
		if ctx.Err() != nil {
			return realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}, true
		}
		if err != nil {
			return s.failed("生成回答", err), true
		}
		s.remember(text, append(trail, moreTrail...), answer)
		done := realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
		if usage != nil {
			done.Usage = &realtimeproto.Usage{InputTokens: usage.PromptTokens, OutputTokens: usage.OutputTokens}
		}
		return done, true
	case confirmNo:
		s.remember(text, nil, "好的，不做了。")
		return s.speakFixedReply(ctx, "好的，不做了。"), true
	default:
		s.logger.Info("用户没有确认，放弃待确认的操作")
		return realtimeproto.ResponseDone{}, false
	}
}
