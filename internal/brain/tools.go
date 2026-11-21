package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

const toolTimeout = 10 * time.Second // 单次工具执行的超时

// lookAtCameraTool 是服务端内置的「看画面」工具：模型调用时才经 MCP 向终端要一帧（终端能拍时才提供）。
// 怎么看、怎么说都写在工具说明里，不写进系统提示词：没有摄像头时这些说明随工具一起不出现，系统提示词不变。
const lookAtCameraTool = "look_at_camera"

// CameraAnswerChars 是看画面后说给主人听的话的上限（字）：语音每秒约五个字，50 字约十秒。
const CameraAnswerChars = 50

var lookAtCameraSpec = ToolSpec{Name: lookAtCameraTool,
	Description: "看摄像头拍到的当前画面（调用时才拍）。只有主人问到看得见的东西时才用（例如：你能看到我吗、我旁边有什么、" +
		"我手里拿着什么、我穿的什么）；没看画面就不要说看到了什么，闲聊时也不要提画面。" +
		fmt.Sprintf("看清形状、颜色和上面的字再下判断。回答一两句、不超过 %d 个字：是什么、把握多大（确定 / 看着像 / 不确定）；", CameraAnswerChars) +
		"外观细节主人问到才说；画面里没有的东西不要编。看不清就说看不清，请主人拿近一点或换个角度。" +
		"画面里直接看到的比搜到的资料可靠，两者对不上时以画面为准，并说不确定。认东西要联网查时，搜索词用外观特征加上画面里读到的字。" +
		"调用前说过的话（例如「我看看」），拿到结果后不要再说一遍。",
	Parameters: ParametersFor[struct{}]()}

// cameraFrameNote 是承载画面的那条消息的文字；frameRecalledNote 是它写进历史时的替身（历史里不存图片）。
const (
	cameraFrameNote   = "这是摄像头当前画面。"
	frameRecalledNote = "（当时看了摄像头画面；画面不保留，现在要看需要重新调用 look_at_camera）"
)

// toolSpecs 返回会话这一轮提供的工具：会话自己的（看画面、换性格、交给强模型、提高上限）+ 工具箱（内置 + MCP）
// + 有待确认的操作时的确认、取消。交给哪个代理时再按它的允许名单筛（Agent.offer）。
func (s *Session) toolSpecs() []ToolSpec {
	var specs []ToolSpec
	if s.parts.Camera != nil && s.parts.Camera.Available() {
		specs = append(specs, lookAtCameraSpec)
	}
	if spec, ok := switchPersonaSpec(s.opts.Personas); ok {
		specs = append(specs, spec)
	}
	if s.parts.Handoff.corrections() {
		specs = append(specs, askStrongerSpec)
	}
	if s.parts.MonthlyCap != nil { // 平时也能口头提高上限（照样过确认闸）
		specs = append(specs, raiseMonthlyCapSpec)
	}
	if s.parts.Tools != nil {
		specs = append(specs, s.parts.Tools.Specs()...)
	}
	return append(specs, s.confirmationSpecs()...) // 有待确认的操作时才有
}

// offeredTools 是系统提示词里介绍的工具：这一轮实际提供的，不含只在待确认时才出现的确认、取消（它们来来去去，
// 写进系统提示词会让提示词跟着变；它们的用法在自己的说明里）。
func (s *Session) offeredTools() []ToolSpec {
	specs := s.toolSpecs()
	return slices.DeleteFunc(specs, func(spec ToolSpec) bool {
		return spec.Name == confirmActionTool || spec.Name == cancelActionTool
	})
}

// toolOverview 是系统提示词里「手边有哪些工具」那一行：从实际提供的清单生成，没提供的不出现（P5）。
func toolOverview(specs []ToolSpec) string {
	if len(specs) == 0 {
		return ""
	}
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return "你手边的工具：" + strings.Join(names, "、") + "。"
}

// expertReport 是专家看完后交给前台的工具结果（转述的要求只放在这条结果里，不改系统提示词，免得提示词缓存失效）。
// 这一轮只调用了看画面时，专家那句由代码直接念给主人（工具结果就是这一轮的回答），结果里记下已经说了什么；
// 同时调用了别的工具时，由前台把专家那句和别的结果一起说。细节都留在历史里，主人追问时用。
func expertReport(report FrameReport, spokenDirectly bool) string {
	switch {
	case report.Spoken == "":
		return expertDetailsOnlyNote + report.Details
	case spokenDirectly:
		return expertSaidNote + report.Spoken + expertDetailsHeading + report.Details
	default:
		return expertSpokenNote + report.Spoken + expertDetailsHeading + report.Details
	}
}

var (
	expertSaidNote   = "看图更准的专家看了当前画面，已经直接把这句告诉主人：\n"
	expertRecordNote = "看图更准的专家看了当前画面，它的结论："
	expertSpokenNote = "看图更准的专家看了当前画面，替你写好了说给主人听的话。照这句说（不用提专家；可以带上你的口吻，" +
		"但不加内容、不说得更长；调用前说过的话不要再说一遍）：\n"
	expertDetailsHeading = "\n\n专家看到的细节和依据（主人追问时用，不要念出来；它已经按需要联网核对过，不要再为画面里的东西另外去搜）：\n"
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

// cameraLook 是一次看画面的结果。
type cameraLook struct {
	frame  []byte       // 拍到的画面；没拍成时为空
	report *FrameReport // 专家的结论；没有专家、专家出错或没拍成时为 nil
	result string       // 交给模型的工具结果（没拍成、或专家出错退回让前台看时）
}

// lookAtCamera 经 MCP 让终端现拍一帧。有看画面的专家时先问专家；专家出错时退回把画面直接交给前台模型。
// 画面不存盘、不进历史。
func (s *Session) lookAtCamera(ctx context.Context, view cameraView) cameraLook {
	if s.parts.Camera == nil {
		return cameraLook{result: "这个终端没有摄像头，看不到。"}
	}
	started := time.Now()
	frame, err := s.parts.Camera.Capture(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return cameraLook{result: "没看成（被打断了）。"}
		}
		s.logger.Warn("调用工具", "tool", lookAtCameraTool, "err", err, "elapsed", since(started))
		return cameraLook{result: captureFailedNote(err)}
	}
	s.logger.Info("拍了一帧", "frame_bytes", len(frame), "elapsed", since(started))
	if s.parts.FrameExpert != nil {
		question := FrameQuestion{Frame: frame, Request: view.request}
		if persona := s.currentPersona(); persona != nil {
			question.Style = persona.Prompt
		}
		report, err := s.examineFrame(ctx, question)
		if err == nil {
			s.logger.Info("调用工具", "tool", lookAtCameraTool, "frame_bytes", len(frame), "by", "专家",
				"spoken_chars", utf8.RuneCountInString(report.Spoken))
			return cameraLook{frame: frame, report: &report}
		}
		if ctx.Err() != nil {
			return cameraLook{result: "没看成（被打断了）。"}
		}
		s.logger.Warn("看画面的专家出错：这次把画面直接交给前台模型", "err", err)
	}
	s.logger.Info("调用工具", "tool", lookAtCameraTool, "frame_bytes", len(frame))
	return cameraLook{frame: frame, result: "已拿到摄像头当前画面，见下一条消息。"}
}

// examineFrame 把画面交给看画面的专家（子任务，经 EnterAgent 记进代理链）。
func (s *Session) examineFrame(ctx context.Context, question FrameQuestion) (FrameReport, error) {
	ctx, err := EnterAgent(ctx, s.logger, s.parts.FrameExpert.AgentName(), Subtask)
	if err != nil {
		return FrameReport{}, err
	}
	return s.parts.FrameExpert.ExamineFrame(ctx, question)
}

// frameMessages 是承载画面的用户消息：只有画面要交给前台模型看时才有（由专家看了、或没拍成时为空）。
func (look cameraLook) frameMessages() []ChatMessage {
	if look.report != nil || len(look.frame) == 0 {
		return nil
	}
	return []ChatMessage{{Role: RoleUser, Text: cameraFrameNote, Image: look.frame}}
}

// speakDirectly 判断专家那句能不能直接当这一轮的回答：这一轮只调用了看画面、专家单独交出了要说的话。
func (look cameraLook) speakDirectly(calls []ToolCall) bool {
	return len(calls) == 1 && look.report != nil && look.report.Spoken != ""
}

// toolResult 是交给模型的工具结果。
func (look cameraLook) toolResult(spokenDirectly bool) string {
	if look.report != nil {
		return expertReport(*look.report, spokenDirectly)
	}
	return look.result
}

// historyResult 是历史里这次看画面记成的样子：专家的结论和细节（主人追问时用），不带「照这句说」这类只对这一轮的要求。
func (look cameraLook) historyResult(spokenDirectly bool) string {
	switch {
	case look.report == nil:
		return look.result
	case spokenDirectly:
		return expertReport(*look.report, true) // 已经是记录：告诉了主人什么 + 细节
	case look.report.Spoken == "":
		return expertRecordNote + look.report.Details
	default:
		return expertRecordNote + "「" + look.report.Spoken + "」" + expertDetailsHeading + look.report.Details
	}
}

// callTool 执行一次工具调用，返回交给模型的结果文字；出错时把错误也交给模型，让它如实告诉用户。
// record 是这一轮的记录：工具用 OnDelivered 登记的事记在这里。
func (s *Session) callTool(ctx context.Context, record *turnRecord, call ToolCall) string {
	ctx, span := tracer.Start(ctx, "brain.tool")
	defer span.End()
	ctx, cancel := context.WithTimeout(WithDeliveries(WithSpeaker(ctx, s.currentConversation().Person()), record), toolTimeout)
	defer cancel()
	started := time.Now()
	result, err := s.parts.Tools.Call(ctx, call)
	span.SetAttributes(semconv.GenAIOperationNameExecuteTool, semconv.GenAIToolName(call.Name), semconv.GenAIToolCallID(call.ID))
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
