package brain

import "encoding/json"

// ProviderReplay 是模型服务要求下一次请求原样带回的内容（例如 Claude 开着思考时的思考块和签名）。只有产生它的适配层
// 看得懂：brain 不解读、不修改，只把它跟着那条回答（ChatMessage.Replay）带到下一次请求里，别的适配层不带。
//
// 带回去的前提是这条回答之前发出去的东西一字不差：系统提示词、工具清单、前面的消息改过（压缩、删改、换了工具）
// 之后再带回，模型服务会拒绝整个请求。所以只在消息只往后加、工具清单不变的地方带（一个调查任务之内），不进对话历史；
// 某一次请求要换工具清单时，用 WithoutReplay 去掉。
type ProviderReplay struct {
	Provider string          // 产生它的适配层
	Data     json.RawMessage // 原样内容，格式由那个适配层定
}

// WithoutReplay 是去掉了 Replay 的一份消息（原来的不动）：给这一次请求用，例如它的工具清单和前几次不同。
func WithoutReplay(messages []ChatMessage) []ChatMessage {
	stripped := make([]ChatMessage, len(messages))
	for i, message := range messages {
		message.Replay = nil
		stripped[i] = message
	}
	return stripped
}
