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
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/bukahou/haro/internal/brain"
)

// OpenAIChatModel 调用 OpenAI 兼容的 Chat Completions 接口（官方 openai-go，流式）：OpenAI、Gemini、通义千问、
// OpenRouter、vLLM、Ollama 的 OpenAI 兼容端点都可以。换供应商只改配置；各家的差别（工具调用编号的格式、
// 认不认「不许调用工具」、要不要预热）是这个适配的选项，不在上层判断。
type OpenAIChatModel struct {
	client            openai.Client
	model             string
	maxTokens         int
	toolCallIDs       ToolCallIDStyle
	omitForbiddenTool bool // 服务端不认 tool_choice:"none"：不许调用工具的一轮干脆不带工具
	preload           bool // 模型按需加载：预热时发一个最小请求，把模型装进显存
	firstTokenTimeout time.Duration
	requestTimeout    time.Duration
}

// OpenAIOptions 是创建 OpenAIChatModel 的参数。
type OpenAIOptions struct {
	BaseURL    string // 例如 https://api.openai.com/v1
	Model      string
	APIKey     string // Bearer；空 = 不带（本地服务）。不读 SDK 默认的 OPENAI_API_KEY 等环境变量
	MaxTokens  int
	HTTPClient *http.Client
	// ToolCallIDs 是服务端要求的工具调用编号格式；空 = 不挑格式（ToolCallIDsOpaque）
	ToolCallIDs ToolCallIDStyle
	// MaxRetries 是 SDK 在 429 / 5xx / 断线时自己重试几次。当主模型时用 0：等不到就交给退路
	MaxRetries int
	// FirstTokenTimeout 是等模型开始回答（第一个流式事件）最多多久，超时就取消、交给退路；0 = 不限
	FirstTokenTimeout time.Duration
	// RequestTimeout 是一次请求从发出到流结束最多多久；0 = 只受调用方的 context 管
	RequestTimeout time.Duration
}

// ToolCallIDStyle 是服务端要求的工具调用编号格式：发出去之前，历史里的编号一律按它重写（调用与结果同步改），
// 所以换供应商、云端本地之间切换都不会因为编号出错。
type ToolCallIDStyle string

const (
	ToolCallIDsOpaque  ToolCallIDStyle = "opaque"  // 不挑格式：写成 call_N
	ToolCallIDsKimi    ToolCallIDStyle = "kimi"    // Kimi（Moonshot）：functions.<工具名>:<序号>；不是这个格式时静默无输出
	ToolCallIDsMistral ToolCallIDStyle = "mistral" // Mistral：恰好 9 位字母数字，否则 400
)

// ParseToolCallIDStyle 解析配置里的编号格式；空 = 不挑格式。
func ParseToolCallIDStyle(value string) (ToolCallIDStyle, error) {
	switch style := ToolCallIDStyle(strings.TrimSpace(value)); style {
	case "", ToolCallIDsOpaque:
		return ToolCallIDsOpaque, nil
	case ToolCallIDsKimi, ToolCallIDsMistral:
		return style, nil
	default:
		return "", fmt.Errorf("工具调用编号格式 %q 不认识（可选 opaque、kimi、mistral）", value)
	}
}

// NewOpenAIChatModel 创建客户端（不发请求）。
func NewOpenAIChatModel(options OpenAIOptions) *OpenAIChatModel {
	style := options.ToolCallIDs
	if style == "" {
		style = ToolCallIDsOpaque
	}
	requestOptions := []option.RequestOption{
		option.WithBaseURL(options.BaseURL),
		option.WithAPIKey(options.APIKey), // 空也要设：盖掉 SDK 从环境变量读到的别处的 key
		option.WithMaxRetries(options.MaxRetries),
	}
	if options.HTTPClient != nil {
		requestOptions = append(requestOptions, option.WithHTTPClient(options.HTTPClient))
	}
	return &OpenAIChatModel{client: openai.NewClient(requestOptions...), model: options.Model, maxTokens: options.MaxTokens,
		toolCallIDs: style, firstTokenTimeout: options.FirstTokenTimeout, requestTimeout: options.RequestTimeout}
}

// NewOllamaChatModel 是本地 Ollama，走它的 OpenAI 兼容端点（baseURL 是 Ollama 的根地址，例如 http://127.0.0.1:11434）：
//   - 不认 tool_choice:"none"（实测照样调用工具）⇒ 不许调用工具的一轮不带工具（本地没有按提示词缓存计费的问题）；
//   - 模型按需加载 ⇒ 预热时发一个最小请求；
//   - 上下文长度、模型在显存里留多久在 Ollama 那边配（OLLAMA_CONTEXT_LENGTH、OLLAMA_KEEP_ALIVE），这个端点不收这两个参数。
func NewOllamaChatModel(baseURL, model string, maxTokens int, options OpenAIOptions) *OpenAIChatModel {
	options.BaseURL, options.Model, options.MaxTokens, options.APIKey = strings.TrimRight(baseURL, "/")+"/v1", model, maxTokens, ""
	chat := NewOpenAIChatModel(options)
	chat.omitForbiddenTool, chat.preload = true, true
	return chat
}

// Warmup 按需加载模型的服务（Ollama）：发一个只要 1 个 token 的请求，让它把模型装进显存；别的服务什么都不做。
func (m *OpenAIChatModel) Warmup(ctx context.Context) error {
	if !m.preload {
		return nil
	}
	_, err := m.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{Model: m.model, MaxTokens: openai.Int(1),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("你好")}})
	return err
}

// StreamReply 流式产出文字；工具调用、用量和停下的原因在流结束时一次性产出（各家的增量格式只保证在结束时完整）。
func (m *OpenAIChatModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		params, err := m.params(request)
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		deadline := newStreamDeadline(ctx, m.requestTimeout, m.firstTokenTimeout)
		defer deadline.close()
		stream := m.client.Chat.Completions.NewStreaming(deadline.ctx, params)
		defer stream.Close() // 调用方提前 break 时关掉连接，即中止生成
		var accumulated openai.ChatCompletionAccumulator
		usageSeen, thinkingChars := false, 0
		for stream.Next() {
			deadline.started()
			chunk := stream.Current()
			if !accumulated.AddChunk(chunk) {
				yield(brain.ReplyDelta{Usage: unknownUsage()}, fmt.Errorf("模型的流式响应前后对不上（%s）", m.model))
				return
			}
			usageSeen = usageSeen || chunk.JSON.Usage.Valid()
			for _, choice := range chunk.Choices {
				thinkingChars += reasoningChars(choice.Delta)
				if choice.Delta.Content != "" && !yield(brain.ReplyDelta{Text: choice.Delta.Content}, nil) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil { // 用量要到流的最后才报：出错、被取消时不知道
			if deadline.noFirstToken() {
				err = fmt.Errorf("%w（等了 %v）", errNoFirstToken, m.firstTokenTimeout)
			}
			yield(brain.ReplyDelta{Usage: unknownUsage()}, fmt.Errorf("请求模型（%s）: %w", m.model, err))
			return
		}
		usage := usageFromOpenAI(accumulated.Usage, usageSeen, thinkingChars)
		if len(accumulated.Choices) == 0 {
			yield(brain.ReplyDelta{Usage: usage}, fmt.Errorf("模型（%s）没有给出回答", m.model))
			return
		}
		choice := accumulated.Choices[0]
		switch choice.FinishReason {
		case "length": // 截断：不交出工具调用（写到一半的参数不完整）
			yield(brain.ReplyDelta{Usage: usage, Stop: brain.StopMaxTokens}, nil)
			return
		case "content_filter":
			yield(brain.ReplyDelta{Usage: usage}, &RefusalError{Category: "content_filter", Explanation: choice.Message.Refusal})
			return
		}
		calls, err := toolCallsFromOpenAI(choice.Message.ToolCalls)
		if err != nil {
			yield(brain.ReplyDelta{Usage: usage}, err)
			return
		}
		yield(brain.ReplyDelta{ToolCalls: calls, Usage: usage}, nil)
	}
}

// params 组装一次请求。
func (m *OpenAIChatModel) params(request brain.ChatRequest) (openai.ChatCompletionNewParams, error) {
	params := openai.ChatCompletionNewParams{
		Model:         m.model,
		Messages:      toOpenAIMessages(request.Messages, m.toolCallIDs),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	if m.maxTokens > 0 {
		params.MaxTokens = openai.Int(int64(m.maxTokens)) // max_tokens：兼容接口普遍认它
	}
	tools := request.Tools
	if request.ForbidToolCalls && m.omitForbiddenTool {
		tools = nil
	}
	if len(tools) == 0 {
		return params, nil
	}
	converted, err := toOpenAITools(tools)
	if err != nil {
		return params, err
	}
	params.Tools = converted
	if request.ForbidToolCalls { // 工具定义照给（提示词缓存的前缀不变），这一轮不许调用
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String(string(openai.ChatCompletionToolChoiceOptionAutoNone))}
	}
	return params, nil
}

// reasoningChars 是这一段里思考内容的字数：有的服务把思考放在单独的字段里（Ollama、OpenRouter 叫 reasoning，
// DeepSeek、vLLM 叫 reasoning_content），只计字数、不念。
func reasoningChars(delta openai.ChatCompletionChunkChoiceDelta) int {
	chars := 0
	for _, key := range []string{"reasoning", "reasoning_content"} {
		field, ok := delta.JSON.ExtraFields[key]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal([]byte(field.Raw()), &text) == nil {
			chars += utf8.RuneCountInString(text)
		}
	}
	return chars
}

// usageFromOpenAI 换算用量：prompt_tokens 是输入的总数（含命中缓存的 cached_tokens，价格不同、分开记）。
// 服务端没报用量（不支持 include_usage）时 token 记成不知道。
func usageFromOpenAI(usage openai.CompletionUsage, seen bool, thinkingChars int) *brain.ReplyUsage {
	if !seen {
		return &brain.ReplyUsage{TokensUnknown: true, ThinkingChars: thinkingChars}
	}
	return &brain.ReplyUsage{PromptTokens: int(usage.PromptTokens), OutputTokens: int(usage.CompletionTokens),
		CacheReadTokens: int(usage.PromptTokensDetails.CachedTokens), ThinkingChars: thinkingChars}
}

// unknownUsage 是请求没正常结束时的用量：OpenAI 兼容接口在流的最后才报用量，所以什么都不知道。
func unknownUsage() *brain.ReplyUsage {
	return &brain.ReplyUsage{Incomplete: true, TokensUnknown: true}
}

// toolCallsFromOpenAI 取出回答里的工具调用（参数解析成对象）。
func toolCallsFromOpenAI(calls []openai.ChatCompletionMessageToolCallUnion) ([]brain.ToolCall, error) {
	converted := make([]brain.ToolCall, 0, len(calls))
	for _, call := range calls {
		arguments := map[string]any{}
		if text := strings.TrimSpace(call.Function.Arguments); text != "" {
			if err := json.Unmarshal([]byte(text), &arguments); err != nil {
				return nil, fmt.Errorf("工具 %s 的参数不是合法 JSON: %w", call.Function.Name, err)
			}
		}
		converted = append(converted, brain.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
	}
	return converted, nil
}

// ——— 消息转换 ———

// toOpenAIMessages 转换消息；历史里的工具调用编号一律按目标服务的格式重写（调用与结果同步改）。
func toOpenAIMessages(messages []brain.ChatMessage, style ToolCallIDStyle) []openai.ChatCompletionMessageParamUnion {
	rewritten := map[string]string{}
	newID := func(original, toolName string) string {
		if id, ok := rewritten[original]; ok {
			return id
		}
		n := len(rewritten)
		var id string
		switch style {
		case ToolCallIDsKimi:
			id = "functions." + toolName + ":" + strconv.Itoa(n)
		case ToolCallIDsMistral:
			id = fmt.Sprintf("call%05d", n) // 恰好 9 位字母数字
		default:
			id = "call_" + strconv.Itoa(n)
		}
		rewritten[original] = id
		return id
	}

	converted := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, msg := range messages {
		switch {
		case msg.Role == brain.RoleSystem:
			converted = append(converted, openai.SystemMessage(msg.Text))
		case msg.Role == brain.RoleUser && len(msg.Image) > 0:
			converted = append(converted, openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{
				openai.TextContentPart(msg.Text),
				openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
					URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(msg.Image)}),
			}))
		case msg.Role == brain.RoleAssistant && len(msg.ToolCalls) > 0:
			assistant := openai.ChatCompletionAssistantMessageParam{}
			if msg.Text != "" {
				assistant.Content.OfString = openai.String(msg.Text)
			}
			for _, call := range msg.ToolCalls {
				arguments, _ := json.Marshal(call.Arguments)
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{ID: newID(call.ID, call.Name),
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: call.Name, Arguments: string(arguments)}}})
			}
			converted = append(converted, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		case msg.Role == brain.RoleAssistant:
			converted = append(converted, openai.AssistantMessage(msg.Text))
		case msg.Role == brain.RoleTool:
			converted = append(converted, openai.ToolMessage(msg.Text, newID(msg.ToolCallID, msg.ToolName)))
		default: // 用户（含系统合成的情况说明）
			converted = append(converted, openai.UserMessage(msg.Text))
		}
	}
	return converted
}

// toOpenAITools 转换工具定义（参数是 JSON Schema）。
func toOpenAITools(tools []brain.ToolSpec) ([]openai.ChatCompletionToolUnionParam, error) {
	converted := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		parameters := shared.FunctionParameters{"type": "object", "properties": map[string]any{}}
		if len(tool.Parameters) > 0 {
			if err := json.Unmarshal(tool.Parameters, &parameters); err != nil {
				return nil, fmt.Errorf("工具 %s 的参数定义不是合法 JSON: %w", tool.Name, err)
			}
		}
		definition := shared.FunctionDefinitionParam{Name: tool.Name, Parameters: parameters}
		if tool.Description != "" {
			definition.Description = openai.String(tool.Description)
		}
		converted = append(converted, openai.ChatCompletionFunctionTool(definition))
	}
	return converted, nil
}
