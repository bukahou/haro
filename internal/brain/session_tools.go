package brain

import (
	"slices"
	"time"
)

// 会话自己提供的工具（不在工具箱里）只登记在 sessionTools 这一张表：这一轮提供哪些、说明、要不要过确认闸、
// 是不是确认 / 取消这类闸门工具，都从这里查（toolSpecs、offeredTools、executable、UnknownTools、requiresConfirmation）。
// 怎么执行各不相同，在 generateSentences 的分发里。

// sessionTool 是会话自己提供的一个工具。
type sessionTool struct {
	name string
	// offer 返回这一轮提供的说明；false = 这一轮不提供（例如终端不能拍、只有一种性格）
	offer func(s *Session) (ToolSpec, bool)
	// gate：确认、取消。只在有待确认的操作时提供，不列进系统提示词的工具介绍（来来去去会让提示词跟着变）；
	// 没提供时模型照着名字调用也执行，交给它们自己核对并说明为什么不执行
	gate bool
}

var sessionTools = []sessionTool{
	{name: lookAtCameraTool, offer: func(s *Session) (ToolSpec, bool) {
		return lookAtCameraSpec, s.parts.Camera != nil && s.parts.Camera.Available()
	}},
	{name: switchPersonaTool, offer: func(s *Session) (ToolSpec, bool) { return switchPersonaSpec(s.opts.Personas) }},
	{name: askStrongerTool, offer: func(s *Session) (ToolSpec, bool) { return askStrongerSpec, s.parts.Handoff.corrections() }},
	// 平时也能口头提高上限（照样过确认闸）
	{name: raiseMonthlyCapTool, offer: func(s *Session) (ToolSpec, bool) { return raiseMonthlyCapSpec, s.parts.MonthlyCap != nil }},
	{name: confirmActionTool, gate: true, offer: func(s *Session) (ToolSpec, bool) { return confirmActionSpec, s.hasPendingActions() }},
	{name: cancelActionTool, gate: true, offer: func(s *Session) (ToolSpec, bool) { return cancelActionSpec, s.hasPendingActions() }},
}

// sessionToolSpecs 是这一轮提供的会话工具（gate 决定取闸门工具还是其余的）。
func (s *Session) sessionToolSpecs(gate bool) []ToolSpec {
	var specs []ToolSpec
	for _, tool := range sessionTools {
		if tool.gate != gate {
			continue
		}
		if spec, ok := tool.offer(s); ok {
			specs = append(specs, spec)
		}
	}
	return specs
}

// hasPendingActions 判断当前对话有没有还在等主人确认的操作。
func (s *Session) hasPendingActions() bool {
	return s.currentConversation().hasPendingActions(time.Now())
}

// isGateTool 判断是不是确认 / 取消这类闸门工具。
func isGateTool(name string) bool {
	return slices.ContainsFunc(sessionTools, func(tool sessionTool) bool { return tool.gate && tool.name == name })
}

// sessionToolNames 是全部会话工具的名字（定义里的工具名要认识）。
func sessionToolNames() []string {
	names := make([]string, len(sessionTools))
	for i, tool := range sessionTools {
		names[i] = tool.name
	}
	return names
}

// sessionToolRequiresConfirmation 判断会话工具 name 要不要过确认闸（看它的说明上的标注）；不是会话工具时 ok 为 false。
func (s *Session) sessionToolRequiresConfirmation(name string) (required, ok bool) {
	for _, tool := range sessionTools {
		if tool.name == name {
			spec, _ := tool.offer(s)
			return spec.RequiresConfirmation, true
		}
	}
	return false, false
}
