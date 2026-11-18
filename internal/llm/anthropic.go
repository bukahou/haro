package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/bukahou/haro/internal/brain"
)

// AnthropicChatModel 调用 Claude API 的 Messages 接口（官方 Go SDK，流式；v7 D2）。
//
// 认证（v7 D3）：配了 APIKey 就用它；没配时交给 SDK 自己找凭证 —— 集群里是 Workload Identity Federation
// （ANTHROPIC_FEDERATION_RULE_ID 等环境变量 + 投射的服务账号令牌，SDK 换成短期令牌并自动续期），不放任何长期密钥。
// ⚠️ 开发机上别用 ANTHROPIC_API_KEY 这个变量名放 key：Claude Code 也读它，开发机的 *.env 会进每个终端。
//
// 思考是关掉的：工具往来时，思考块（带签名）必须原样回传，而 brain.ChatMessage 带不回原始内容块；
// 关掉也最快（聊天 4 秒内要开始回答）。回答花多少力气由 Effort 管。
// 提示词缓存：系统提示词末尾一个断点（工具定义 + 系统提示词）+ 请求级自动断点（对话历史逐轮累积）。
type AnthropicChatModel struct {
	client    anthropic.Client
	model     string
	maxTokens int
	effort    anthropic.OutputConfigEffort
}

// AnthropicOptions 是创建 AnthropicChatModel 的参数。
type AnthropicOptions struct {
	Model     string // 例如 claude-haiku-5-5
	MaxTokens int    // 每次最多输出多少 token
	Effort    string // low / medium / high（思考关掉时只能用这三档）；空 = 模型默认
	APIKey    string // 空 = 交给 SDK 的凭证链（集群里是 WIF）
	BaseURL   string // 空 = https://api.anthropic.com
	// MaxRetries 是 SDK 在 429 / 5xx / 断线时自己重试几次。当主模型时用 0：4 秒内没开始回答就退回本地，
	// 不要在这里多等；后台调查不急，可以重试。
	MaxRetries int
	HTTPClient *http.Client // 超时由调用方控制（主模型用等响应头最多 HARO_CLOUD_LLM_TIMEOUT 的客户端）
}

// NewAnthropicChatModel 创建客户端（不发请求；凭证在第一次请求时才用到）。
func NewAnthropicChatModel(options AnthropicOptions) *AnthropicChatModel {
	requestOptions := []option.RequestOption{option.WithMaxRetries(options.MaxRetries)}
	if options.HTTPClient != nil {
		requestOptions = append(requestOptions, option.WithHTTPClient(options.HTTPClient))
	}
	if options.APIKey != "" {
		requestOptions = append(requestOptions, option.WithAPIKey(options.APIKey))
	}
	if options.BaseURL != "" {
		requestOptions = append(requestOptions, option.WithBaseURL(options.BaseURL))
	}
	return &AnthropicChatModel{client: anthropic.NewClient(requestOptions...), model: options.Model,
		maxTokens: options.MaxTokens, effort: anthropic.OutputConfigEffort(options.Effort)}
}

// Warmup 云端不需要预热。
func (m *AnthropicChatModel) Warmup(context.Context) error { return nil }

// RefusalError 是 Claude 的安全审核拒绝回答（HTTP 200，stop_reason = refusal）。
// 当作云端失败处理：开着退路时这一轮改用本地模型（v7：用户按推荐）；没有退路时念 SpokenMessage。
type RefusalError struct {
	Category    string
	Explanation string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("Claude 拒绝回答（%s）：%s", e.Category, e.Explanation)
}

// SpokenMessage 见 brain.SpokenError。
func (e *RefusalError) SpokenMessage() string { return "这个问题我这边回答不了。" }

// StreamReply 流式产出文字；工具调用和用量在流结束时一次性产出（与 OpenAIChatModel 相同）。
func (m *AnthropicChatModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		params, err := m.request(messages, tools)
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		stream := m.client.Messages.NewStreaming(ctx, params)
		defer stream.Close() // 调用方提前 break 时关掉连接，即中止生成
		var message anthropic.Message
		for stream.Next() {
			event := stream.Current()
			if err := message.Accumulate(event); err != nil {
				yield(brain.ReplyDelta{}, fmt.Errorf("解析 Claude 的流式响应: %w", err))
				return
			}
			blockDelta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent)
			if !ok {
				continue
			}
			if text, ok := blockDelta.Delta.AsAny().(anthropic.TextDelta); ok && text.Text != "" {
				if !yield(brain.ReplyDelta{Text: text.Text}, nil) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(brain.ReplyDelta{}, fmt.Errorf("请求 Claude: %w", err))
			return
		}
		usage := usageOf(message.Usage)
		if message.StopReason == anthropic.StopReasonRefusal {
			// 带上用量：被拒的请求照样计费，记账的装饰器先记下这一笔，退路再接手
			yield(brain.ReplyDelta{Usage: usage}, &RefusalError{Category: string(message.StopDetails.Category),
				Explanation: message.StopDetails.Explanation})
			return
		}
		calls, err := toolCallsOf(message.Content)
		if err != nil {
			yield(brain.ReplyDelta{Usage: usage}, err)
			return
		}
		yield(brain.ReplyDelta{ToolCalls: calls, Usage: usage}, nil)
	}
}

// request 组装一次请求。
func (m *AnthropicChatModel) request(messages []brain.ChatMessage, tools []brain.ToolSpec) (anthropic.MessageNewParams, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(m.model),
		MaxTokens: int64(m.maxTokens),
		Thinking:  anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}},
		// 请求级自动缓存：断点落在最后一块，对话历史逐轮累积进缓存（下一轮只有新的部分按全价算）
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
	if m.effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: m.effort}
	}
	system, converted := toAnthropicMessages(messages)
	if system != "" {
		// 第二个断点：工具定义 + 系统提示词。对话窗口滑动（最早一轮掉出去）时它们照样命中缓存
		params.System = []anthropic.TextBlockParam{{Text: system, CacheControl: anthropic.NewCacheControlEphemeralParam()}}
	}
	params.Messages = converted
	if len(tools) == 0 {
		// 这一轮不许调用工具（例如最后一轮逼模型直接回答），但历史里可能有工具往来：
		// Claude 要求有工具往来的请求带上工具定义，所以按历史里的工具名补上，并声明这一轮不许调用
		tools = toolsSeenIn(messages)
		if len(tools) > 0 {
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		}
	}
	if len(tools) > 0 {
		converted, err := toAnthropicTools(tools)
		if err != nil {
			return params, err
		}
		params.Tools = converted
	}
	return params, nil
}

// ——— 消息转换 ———

// toAnthropicMessages 把 brain 的消息转成 Claude 的格式：
//   - 系统消息合并成顶层 system（haro 只在开头放一条）
//   - 工具结果放进紧随其后的用户消息里（Claude 要求如此），看画面的那条用户消息排在工具结果之后、并进同一条
//   - 连续同角色的消息合并成一条
//   - 工具调用 ID 统一重写成 call_N（Claude 只认字母数字、下划线、横线；本地模型 / 别家给的 ID 不一定合格），调用与结果同步改
//   - 配不上对的工具调用 / 结果（Claude 要求每个调用紧跟着它的结果，缺了直接 400）：调用去掉，结果改成普通文字
func toAnthropicMessages(messages []brain.ChatMessage) (string, []anthropic.MessageParam) {
	answered := answeredToolCalls(messages)
	ids := map[string]string{}
	toolUseID := func(original string) string {
		if id, ok := ids[original]; ok {
			return id
		}
		id := "call_" + strconv.Itoa(len(ids))
		ids[original] = id
		return id
	}

	var system []string
	var converted []anthropic.MessageParam
	addUser := func(blocks ...anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if last := len(converted) - 1; last >= 0 && converted[last].OfUser != nil {
			converted[last].OfUser.Content = append(converted[last].OfUser.Content, blocks...)
			return
		}
		converted = append(converted, anthropic.NewUserMessage(blocks...))
	}
	addAssistant := func(blocks ...anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if last := len(converted) - 1; last >= 0 && converted[last].OfAssistant != nil {
			converted[last].OfAssistant.Content = append(converted[last].OfAssistant.Content, blocks...)
			return
		}
		converted = append(converted, anthropic.NewAssistantMessage(blocks...))
	}
	for index, msg := range messages {
		switch msg.Role {
		case brain.RoleSystem:
			if strings.TrimSpace(msg.Text) != "" {
				system = append(system, msg.Text)
			}
		case brain.RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			if msg.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Text))
			}
			for _, call := range msg.ToolCalls {
				if !answered[index][call.ID] {
					continue
				}
				input := call.Arguments
				if input == nil {
					input = map[string]any{} // Claude 要求 input 是对象
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(toolUseID(call.ID), input, call.Name))
			}
			addAssistant(blocks...)
		case brain.RoleTool:
			if id, ok := ids[msg.ToolCallID]; ok && pairedResult(messages, index) {
				addUser(anthropic.NewToolResultBlock(id, msg.Text, false))
			} else {
				addUser(anthropic.NewTextBlock(fmt.Sprintf("（工具 %s 的结果）%s", msg.ToolName, msg.Text)))
			}
		default: // 用户
			var blocks []anthropic.ContentBlockParamUnion
			if msg.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Text))
			}
			if len(msg.Image) > 0 {
				blocks = append(blocks, anthropic.NewImageBlockBase64("image/jpeg", base64.StdEncoding.EncodeToString(msg.Image)))
			}
			addUser(blocks...)
		}
	}
	return strings.Join(system, "\n\n"), converted
}

// answeredToolCalls 找出每条助手消息里「后面紧跟着结果」的工具调用：answered[助手消息下标][调用 ID]。
// 结果必须紧跟在调用之后（中间只能是别的工具结果）。
func answeredToolCalls(messages []brain.ChatMessage) map[int]map[string]bool {
	answered := map[int]map[string]bool{}
	for index, msg := range messages {
		if msg.Role != brain.RoleAssistant || len(msg.ToolCalls) == 0 {
			continue
		}
		called := map[string]bool{}
		for _, call := range msg.ToolCalls {
			called[call.ID] = true
		}
		set := map[string]bool{}
		for next := index + 1; next < len(messages) && messages[next].Role == brain.RoleTool; next++ {
			if called[messages[next].ToolCallID] {
				set[messages[next].ToolCallID] = true
			}
		}
		answered[index] = set
	}
	return answered
}

// pairedResult 判断 messages[index]（工具结果）是否紧跟在发起它的那条助手消息之后。
func pairedResult(messages []brain.ChatMessage, index int) bool {
	id := messages[index].ToolCallID
	for previous := index - 1; previous >= 0; previous-- {
		switch messages[previous].Role {
		case brain.RoleTool:
			continue
		case brain.RoleAssistant:
			for _, call := range messages[previous].ToolCalls {
				if call.ID == id {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}

// toolsSeenIn 列出历史里出现过的工具（只有名字）：这一轮不许调用工具、但历史里有工具往来时补上定义用。
func toolsSeenIn(messages []brain.ChatMessage) []brain.ToolSpec {
	seen := map[string]bool{}
	var specs []brain.ToolSpec
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if !seen[call.Name] {
				seen[call.Name] = true
				specs = append(specs, brain.ToolSpec{Name: call.Name})
			}
		}
	}
	return specs
}

// toAnthropicTools 转换工具定义（参数是 JSON Schema：properties / required 放进对应字段，其余原样带上）。
func toAnthropicTools(tools []brain.ToolSpec) ([]anthropic.ToolUnionParam, error) {
	converted := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		schema := anthropic.ToolInputSchemaParam{}
		if len(tool.Parameters) > 0 {
			var fields map[string]any
			if err := json.Unmarshal(tool.Parameters, &fields); err != nil {
				return nil, fmt.Errorf("工具 %s 的参数定义不是合法 JSON: %w", tool.Name, err)
			}
			for key, value := range fields {
				switch key {
				case "type": // 固定是 object
				case "properties":
					schema.Properties = value
				case "required":
					list, _ := value.([]any)
					for _, name := range list {
						if text, ok := name.(string); ok {
							schema.Required = append(schema.Required, text)
						}
					}
				default:
					if schema.ExtraFields == nil {
						schema.ExtraFields = map[string]any{}
					}
					schema.ExtraFields[key] = value
				}
			}
		}
		if schema.Properties == nil {
			// 没有参数的工具（以及按历史补上的工具）也要有 input_schema：全空时 SDK 会省掉这个字段，Claude 回 400
			schema.Properties = map[string]any{}
		}
		wire := anthropic.ToolParam{Name: tool.Name, InputSchema: schema}
		if tool.Description != "" {
			wire.Description = anthropic.String(tool.Description)
		}
		converted = append(converted, anthropic.ToolUnionParam{OfTool: &wire})
	}
	return converted, nil
}

// ——— 响应转换 ———

// toolCallsOf 取出回答里的工具调用（参数解析成对象）。
func toolCallsOf(content []anthropic.ContentBlockUnion) ([]brain.ToolCall, error) {
	var calls []brain.ToolCall
	for _, block := range content {
		use, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		arguments := map[string]any{}
		if text := strings.TrimSpace(string(use.Input)); text != "" && text != "null" {
			if err := json.Unmarshal(use.Input, &arguments); err != nil {
				return nil, fmt.Errorf("工具 %s 的参数不是合法 JSON: %w", use.Name, err)
			}
		}
		calls = append(calls, brain.ToolCall{ID: use.ID, Name: use.Name, Arguments: arguments})
	}
	return calls, nil
}

// usageOf 换算用量：PromptTokens 是输入的总数（没命中缓存的 + 缓存读 + 缓存写），三者价格不同，分开记。
func usageOf(usage anthropic.Usage) *brain.ReplyUsage {
	read, write := int(usage.CacheReadInputTokens), int(usage.CacheCreationInputTokens)
	return &brain.ReplyUsage{PromptTokens: int(usage.InputTokens) + read + write, OutputTokens: int(usage.OutputTokens),
		CacheReadTokens: read, CacheWriteTokens: write}
}
