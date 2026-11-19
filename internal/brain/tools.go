package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"

	"github.com/bukahou/haro/internal/realtimeproto"
)

const (
	maxToolRounds = 3                // 一句话最多几轮工具调用（防止小模型陷入循环）
	toolTimeout   = 10 * time.Second // 单次工具执行的超时
)

// lookAtCameraTool 是服务端内置的「看画面」工具：模型调用时才经 MCP 向终端要一帧（终端能拍时才提供）。
// 怎么看、怎么说都写在工具说明里，不写进系统提示词：没有摄像头时这些说明随工具一起不出现，系统提示词不变。
const lookAtCameraTool = "look_at_camera"

// CameraAnswerChars 是看画面后说给主人听的话的上限（字）：语音每秒约五个字，50 字约十秒。
const CameraAnswerChars = 50

var lookAtCameraSpec = ToolSpec{Name: lookAtCameraTool,
	Description: "看摄像头拍到的当前画面（调用时才拍）。只有主人问到看得见的东西时才用（例如：你能看到我吗、我旁边有什么、" +
		"我手里拿着什么、我穿的什么）；没看画面就不要说看到了什么。" +
		fmt.Sprintf("看清形状、颜色和上面的字再下判断。回答一两句、不超过 %d 个字：是什么、把握多大（确定 / 看着像 / 不确定）；", CameraAnswerChars) +
		"外观细节主人问到才说；画面里没有的东西不要编。看不清就说看不清，请主人拿近一点或换个角度。" +
		"画面里直接看到的比搜到的资料可靠，两者对不上时以画面为准，并说不确定。认东西要联网查时，搜索词用外观特征加上画面里读到的字。" +
		"调用前说过的话（例如「我看看」），拿到结果后不要再说一遍。",
	Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}

// cameraFrameNote 是承载画面的那条消息的文字；frameRecalledNote 是它写进历史时的替身（历史里不存图片）。
const (
	cameraFrameNote   = "这是摄像头当前画面。"
	frameRecalledNote = "（当时看了摄像头画面；画面不保留，现在要看需要重新调用 look_at_camera）"
)

// toolSpecs 返回可用的工具清单：内置的看画面 + 工具箱（内置时间 + MCP）。
func (s *Session) toolSpecs() []ToolSpec {
	var specs []ToolSpec
	if s.parts.Camera != nil && s.parts.Camera.Available() {
		specs = append(specs, lookAtCameraSpec)
	}
	if spec, ok := switchPersonaSpec(s.opts.Personas); ok {
		specs = append(specs, spec)
	}
	if s.opts.Escalation {
		specs = append(specs, askStrongerSpec)
	}
	if s.parts.Tools != nil {
		specs = append(specs, s.parts.Tools.Specs()...)
	}
	return specs
}

// expertReport 是专家看完后交给前台的工具结果（转述的要求只放在这条结果里，不改系统提示词，免得提示词缓存失效）：
// 专家已经写好说给主人听的那句，前台照着说；细节留在历史里，主人追问时用。
func expertReport(report FrameReport) string {
	if report.Spoken == "" {
		return expertDetailsOnlyNote + report.Details
	}
	return expertSpokenNote + report.Spoken + expertDetailsHeading + report.Details
}

var (
	expertSpokenNote = "看图更准的专家看了当前画面，替你写好了说给主人听的话。照这句说（不用提专家；可以带上你的口吻，" +
		"但不加内容、不说得更长；调用前说过的话不要再说一遍）：\n"
	expertDetailsHeading = "\n\n专家看到的细节和依据（主人追问时用，这次不要念出来；它已经按需要联网核对过，不要再为画面里的东西另外去搜）：\n"
	// expertDetailsOnlyNote：专家没把要说的话单独交出来时，前台自己压短。
	expertDetailsOnlyNote = "看图更准的专家看了当前画面，判断如下。用你自己的口吻告诉主人（不用提专家），一两句话、" +
		"不超过 " + strconv.Itoa(CameraAnswerChars) + " 个字：" +
		"它判断是什么、有多大把握，不要说得比它更肯定，不要补充它没说的细节；它请主人拿近或换角度的，照样转达；" +
		"调用前说过的话不要再说一遍。它已经按需要联网核对过，这次不要再为画面里的东西另外去搜。\n\n"
)

// captureFailedNote 是拍失败时交给模型的工具结果：说明没拍成和原因（终端给的说明可以直接转述）。
func captureFailedNote(err error) string {
	return "没拍成：" + err.Error() + "。告诉主人现在看不到。"
}

// lookAtCamera 经 MCP 让终端现拍一帧，返回交给模型的工具结果，以及承载画面的用户消息（拍失败、或由专家看了时为空）。
// 有看画面的专家时先问专家；专家出错时退回把画面直接交给前台模型。画面不存盘、不进历史。
func (s *Session) lookAtCamera(ctx context.Context, view cameraView) (string, []ChatMessage) {
	if s.parts.Camera == nil {
		return "这个终端没有摄像头，看不到。", nil
	}
	started := time.Now()
	frame, err := s.parts.Camera.Capture(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "没看成（被打断了）。", nil
		}
		s.logger.Warn("调用工具", "tool", lookAtCameraTool, "err", err, "elapsed", since(started))
		return captureFailedNote(err), nil
	}
	s.logger.Info("拍了一帧", "frame_bytes", len(frame), "elapsed", since(started))
	if s.parts.FrameExpert != nil {
		report, err := s.parts.FrameExpert.ExamineFrame(ctx, frame, view.request)
		if err == nil {
			s.logger.Info("调用工具", "tool", lookAtCameraTool, "frame_bytes", len(frame), "by", "专家",
				"spoken_chars", utf8.RuneCountInString(report.Spoken))
			return expertReport(report), nil
		}
		if ctx.Err() != nil {
			return "没看成（被打断了）。", nil
		}
		s.logger.Warn("看画面的专家出错：这次把画面直接交给前台模型", "err", err)
	}
	s.logger.Info("调用工具", "tool", lookAtCameraTool, "frame_bytes", len(frame))
	return "已拿到摄像头当前画面，见下一条消息。", []ChatMessage{{Role: RoleUser, Text: cameraFrameNote, Image: frame}}
}

// needsConfirmation 判断这批调用里有没有要用户口头确认的（MCP destructiveHint）。
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
		// 实测：只给错误时，小模型之后主人再问也不再调用，一直说「服务在打瞌睡」
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

// deleteTargets 取出删除工具参数里的描述：queries（一次删多条）或旧的 query。
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
		// 由模型根据执行结果回答：删了哪几条、哪些没找到、失败了为什么，固定的「删掉了」说不清这些。
		history := append(s.historyWithInstructions(), s.userTurn(text))
		history = append(history, trail[:len(trail)-1]...)
		answer, moreTrail, usage, err := s.speakAnswer(ctx, history, trail[len(trail)-1], cameraView{})
		if ctx.Err() != nil {
			return realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}, true
		}
		if err != nil {
			return s.failed(realtimeproto.ReasonGenerationFailed, "生成回答", err), true
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
