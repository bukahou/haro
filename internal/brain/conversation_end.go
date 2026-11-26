package brain

// 一句话结束对话（11-23 真机验收 F7，2026-11-26 用户裁定做）：主人说「好了，你先休息吧」「先这样，拜拜」时，
// 由模型判断他要结束这次对话（P1：代码不看他说了什么），调用 end_conversation；这一轮说完，终端播完就请语音前端休眠，
// 不用等对话窗口到期（response.done 带 end_conversation，协议 v4）。播的时候被打断就不睡。

const endConversationTool = "end_conversation"

var endConversationSpec = ToolSpec{Name: endConversationTool,
	Description: "主人表示这次先聊到这里、让你去休息、跟你道别时调用（例如「好了，你先休息吧」「先这样，拜拜」）。" +
		"调用后只说一句简短的告别；这一轮说完你就睡着，主人要再叫唤醒词才能接着说。主人说的是他自己要去睡觉、并没有让你停时不要调用。",
	Parameters: ParametersFor[struct{}]()}

// endConversationResult 是调用之后交给模型的结果。
const endConversationResult = "好：这一轮说完你就休眠。只说一句简短的告别，不要再问问题。"

// usesWakeWord：这台终端用唤醒门（识别流开着唤醒词）。不用唤醒门时没有「休眠」可言，不提供 end_conversation。
func (s *Session) usesWakeWord() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamConfig != nil && s.streamConfig.WakeWord
}
