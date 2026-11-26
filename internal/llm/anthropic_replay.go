package llm

import (
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/bukahou/haro/internal/brain"
)

// 思考块原样带回：开着思考时，调完工具的下一次请求要把上一次回答里的思考块（带签名）按原来的顺序原样带回，
// 不然模型看不到自己刚才想了什么。回答收齐时把整条回答的块序列记进 brain.ProviderReplay（格式只有这里看得懂），
// 下一次组装这条回答时按原来的顺序放回思考块；文字和工具调用仍按 brain 的消息组装（调用编号照样重写成 call_N，
// 每次请求重写的结果相同，思考块前面的内容就不变）。brain 的消息和记下的对不上时（文字改过、调用不一样）不带思考块，
// 按没开思考的写法组装：少了这次的思考，请求照样合法。

// claudeReplayProvider 是 Claude 适配层记下的 brain.ProviderReplay.Provider。
const claudeReplayProvider = "anthropic"

// claudeReplayBlock 是一条回答里的一块：思考块原样记下；文字和工具调用只记核对和排位用的内容。
type claudeReplayBlock struct {
	Type      string `json:"type"`                  // replayThinking 等
	Thinking  string `json:"thinking,omitempty"`    // 思考的文字（模型默认不返回时是空的，照样带回）
	Signature string `json:"signature,omitempty"`   // 思考块的签名
	Data      string `json:"data,omitempty"`        // 加密的思考块
	Text      string `json:"text,omitempty"`        // 文字
	ToolUseID string `json:"tool_use_id,omitempty"` // 模型给的调用编号（= brain.ToolCall.ID）
}

// 记下的块的种类（和 Claude 的块类型同名）。
const (
	replayThinking         = "thinking"
	replayRedactedThinking = "redacted_thinking"
	replayText             = "text"
	replayToolUse          = "tool_use"
)

// replayOf 是一条回答要原样带回的内容。回答里没有思考块时为 nil；有认不出的块（服务端工具等，haro 不用）时也为 nil：
// 照原样排不回去，不如不带。
func replayOf(content []anthropic.ContentBlockUnion) *brain.ProviderReplay {
	var blocks []claudeReplayBlock
	thinking := false
	for _, block := range content {
		switch block := block.AsAny().(type) {
		case anthropic.ThinkingBlock:
			thinking = true
			blocks = append(blocks, claudeReplayBlock{Type: replayThinking, Thinking: block.Thinking, Signature: block.Signature})
		case anthropic.RedactedThinkingBlock:
			thinking = true
			blocks = append(blocks, claudeReplayBlock{Type: replayRedactedThinking, Data: block.Data})
		case anthropic.TextBlock:
			blocks = append(blocks, claudeReplayBlock{Type: replayText, Text: block.Text})
		case anthropic.ToolUseBlock:
			blocks = append(blocks, claudeReplayBlock{Type: replayToolUse, ToolUseID: block.ID})
		default:
			return nil
		}
	}
	if !thinking {
		return nil
	}
	data, err := json.Marshal(blocks)
	if err != nil { // 字段都是字符串，不会出错
		return nil
	}
	return &brain.ProviderReplay{Provider: claudeReplayProvider, Data: data}
}

// replayedAssistant 按记下的块序列组装一条带思考块的回答：思考块原样、在原来的位置；文字、工具调用按 brain 的消息。
// toolUse 组装一次调用，keep = false 的不带（例如后面没跟着结果的）。记下的不是这个适配层的、解析不了、
// 或者和 brain 的消息对不上时 ok = false，由调用方按没开思考的写法组装。
func replayedAssistant(msg brain.ChatMessage,
	toolUse func(brain.ToolCall) (block anthropic.ContentBlockParamUnion, keep bool)) (blocks []anthropic.ContentBlockParamUnion, ok bool) {
	if msg.Replay == nil || msg.Replay.Provider != claudeReplayProvider {
		return nil, false
	}
	var recorded []claudeReplayBlock
	if err := json.Unmarshal(msg.Replay.Data, &recorded); err != nil {
		return nil, false
	}
	calls := make(map[string]brain.ToolCall, len(msg.ToolCalls))
	for _, call := range msg.ToolCalls {
		calls[call.ID] = call
	}
	var text strings.Builder
	matched := 0 // 记下的调用里 brain 的消息也有的
	for _, block := range recorded {
		switch block.Type {
		case replayThinking:
			blocks = append(blocks, anthropic.NewThinkingBlock(block.Signature, block.Thinking))
		case replayRedactedThinking:
			blocks = append(blocks, anthropic.NewRedactedThinkingBlock(block.Data))
		case replayText:
			text.WriteString(block.Text)
			if block.Text != "" { // Claude 不收空的文字块
				blocks = append(blocks, anthropic.NewTextBlock(block.Text))
			}
		case replayToolUse:
			call, found := calls[block.ToolUseID]
			if !found {
				return nil, false
			}
			matched++
			if param, keep := toolUse(call); keep {
				blocks = append(blocks, param)
			}
		default:
			return nil, false
		}
	}
	if text.String() != msg.Text || matched != len(msg.ToolCalls) {
		return nil, false
	}
	return blocks, true
}
