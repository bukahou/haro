package brain

import "slices"

// 会话自己提供的工具（不在工具箱里）只登记在 sessionTools 这一张表：这一轮提供哪些、说明、要不要过确认闸、
// 是不是确认 / 取消这类闸门工具，都从这里查（toolSpecs、offeredTools、executable、UnknownTools、requiresConfirmation）。
// 怎么执行各不相同，在 generateSentences 的分发里。

// sessionTool 是会话自己提供的一个工具。
type sessionTool struct {
	name string
	// offer 返回这一轮提供的说明；false = 这一轮不提供（例如终端不能拍、只有一种性格）
	offer func(s *Session) (ToolSpec, bool)
	// gate：确认、取消。有会破坏的工具时一直提供（sessionToolSpecs 判断；工具定义在提示词缓存的最前面，
	// 加上、去掉都让整段缓存失效）；没有待确认的操作时调用了照样执行，由它们自己核对并说明为什么不做。
	// 不列进系统提示词的工具介绍：只在问过主人之后才用得上，用法写在它们自己的说明里
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
	{name: confirmActionTool, gate: true, offer: func(*Session) (ToolSpec, bool) { return confirmActionSpec, true }},
	{name: cancelActionTool, gate: true, offer: func(*Session) (ToolSpec, bool) { return cancelActionSpec, true }},
}

// sessionToolSpecs 是这一轮提供的会话工具（gate 决定取闸门工具还是其余的）；闸门工具只在有会破坏的工具时提供。
func (s *Session) sessionToolSpecs(gate bool) []ToolSpec {
	if gate && !s.hasDestructiveTools() {
		return nil
	}
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

// hasDestructiveTools 判断这一轮提供的工具里有没有会破坏的（要过确认闸的：会话自己的、工具箱里的）。
func (s *Session) hasDestructiveTools() bool {
	for _, tool := range sessionTools {
		if tool.gate {
			continue
		}
		if spec, ok := tool.offer(s); ok && spec.RequiresConfirmation {
			return true
		}
	}
	return s.parts.Tools != nil && slices.ContainsFunc(s.parts.Tools.Specs(), func(spec ToolSpec) bool { return spec.RequiresConfirmation })
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
