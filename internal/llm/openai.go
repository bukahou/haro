package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"haro/internal/brain"
)

// OpenAIChatModel 调用任何 OpenAI 兼容的 POST /chat/completions（流式）：
// Bedrock 的 OpenAI 兼容端点、OpenAI、Gemini、通义千问、OpenRouter、vLLM、ollama 都可以。换供应商只改配置。
type OpenAIChatModel struct {
	BaseURL   string // 例如 https://bedrock-runtime.<region>.amazonaws.com/openai/v1
	Model     string
	APIKey    string // Authorization: Bearer
	MaxTokens int
	Client    *http.Client
}

// NewOpenAIChatModel 创建客户端。
func NewOpenAIChatModel(baseURL, model, apiKey string, maxTokens int, client *http.Client) *OpenAIChatModel {
	return &OpenAIChatModel{BaseURL: strings.TrimRight(baseURL, "/"), Model: model, APIKey: apiKey,
		MaxTokens: maxTokens, Client: client}
}

// Warmup 云端不需要预热。
func (m *OpenAIChatModel) Warmup(context.Context) error { return nil }

// ——— 请求 ———

type openAIRequest struct {
	Model         string          `json:"model"`
	Messages      []openAIMessage `json:"messages"`
	Tools         []openAITool    `json:"tools,omitempty"`
	Stream        bool            `json:"stream"`
	StreamOptions map[string]bool `json:"stream_options,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"` // 字符串，或 [{type:text|image_url}]；带 tool_calls 时可以为 null
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"` // JSON 文本（流式时分段到达）
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ——— 流式响应 ———

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// StreamReply 发送消息并流式产出文字；工具调用在流结束时一次性产出（各家的增量格式只保证在结束时完整）。
func (m *OpenAIChatModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		resp, err := m.post(ctx, messages, tools)
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		defer resp.Body.Close()

		calls := map[int]*openAIToolCall{}
		var usage *brain.ReplyUsage
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue // SSE 的空行、注释、event: 行
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				break
			}
			var chunk openAIChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				yield(brain.ReplyDelta{}, fmt.Errorf("解析流式响应: %w", err))
				return
			}
			if chunk.Error != nil {
				yield(brain.ReplyDelta{}, fmt.Errorf("模型服务报错: %s", chunk.Error.Message))
				return
			}
			if chunk.Usage != nil {
				usage = &brain.ReplyUsage{PromptTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
			}
			for _, choice := range chunk.Choices {
				for _, fragment := range choice.Delta.ToolCalls {
					accumulateToolCall(calls, fragment)
				}
				if choice.Delta.Content != "" && !yield(brain.ReplyDelta{Text: choice.Delta.Content}, nil) {
					return // 调用方 break：关闭响应体即中止生成
				}
			}
		}
		if err := scanner.Err(); err != nil {
			yield(brain.ReplyDelta{}, fmt.Errorf("读取流式响应: %w", err))
			return
		}
		finished, err := finishToolCalls(calls)
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		yield(brain.ReplyDelta{ToolCalls: finished, Usage: usage}, nil)
	}
}

func (m *OpenAIChatModel) post(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) (*http.Response, error) {
	payload, err := json.Marshal(openAIRequest{
		Model: m.Model, Messages: toOpenAIMessages(messages, toolCallIDStyleFor(m.Model)), Tools: toOpenAITools(tools),
		Stream: true, StreamOptions: map[string]bool{"include_usage": true}, MaxTokens: m.MaxTokens,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求云端模型: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("云端模型返回 %d: %s", resp.StatusCode, detail)
	}
	return resp, nil
}

// accumulateToolCall 合并流式到达的工具调用片段（同一个 index 的 name / arguments 分段拼接）。
func accumulateToolCall(calls map[int]*openAIToolCall, fragment openAIToolCall) {
	call, ok := calls[fragment.Index]
	if !ok {
		call = &openAIToolCall{Index: fragment.Index}
		calls[fragment.Index] = call
	}
	if fragment.ID != "" {
		call.ID = fragment.ID
	}
	call.Function.Name += fragment.Function.Name
	call.Function.Arguments += fragment.Function.Arguments
}

func finishToolCalls(calls map[int]*openAIToolCall) ([]brain.ToolCall, error) {
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	finished := make([]brain.ToolCall, 0, len(calls))
	for _, index := range indexes {
		call := calls[index]
		arguments := map[string]any{}
		if text := strings.TrimSpace(call.Function.Arguments); text != "" {
			if err := json.Unmarshal([]byte(text), &arguments); err != nil {
				return nil, fmt.Errorf("工具 %s 的参数不是合法 JSON: %w", call.Function.Name, err)
			}
		}
		finished = append(finished, brain.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
	}
	return finished, nil
}

// ——— 消息转换 ———

// toolCallIDStyle 是目标模型要求的工具调用 ID 格式。
type toolCallIDStyle int

const (
	idStyleOpaque  toolCallIDStyle = iota // 不挑格式（Qwen 等）：统一写成 call_N
	idStyleKimi                           // Kimi：functions.<工具名>:<序号>；不是这个格式时会静默无输出
	idStyleMistral                        // Mistral：9 位字母数字，否则 400
)

// toolCallIDStyleFor 按模型 ID 选择 ID 格式（在 Bedrock 上实测的坑）。
func toolCallIDStyleFor(model string) toolCallIDStyle {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "kimi") || strings.Contains(lower, "moonshot"):
		return idStyleKimi
	case strings.Contains(lower, "mistral") || strings.Contains(lower, "ministral"):
		return idStyleMistral
	default:
		return idStyleOpaque
	}
}

// toOpenAIMessages 转换消息；历史里的工具调用 ID 一律按目标模型的格式重写（调用与结果同步改），
// 所以云端 / 本地之间切换、换供应商时都不会因为 ID 格式出错。
func toOpenAIMessages(messages []brain.ChatMessage, style toolCallIDStyle) []openAIMessage {
	rewritten := map[string]string{}
	newID := func(original, toolName string) string {
		if id, ok := rewritten[original]; ok {
			return id
		}
		n := len(rewritten)
		var id string
		switch style {
		case idStyleKimi:
			id = "functions." + toolName + ":" + strconv.Itoa(n)
		case idStyleMistral:
			id = fmt.Sprintf("call%05d", n) // 恰好 9 位字母数字
		default:
			id = "call_" + strconv.Itoa(n)
		}
		rewritten[original] = id
		return id
	}

	converted := make([]openAIMessage, 0, len(messages))
	for _, msg := range messages {
		item := openAIMessage{Role: string(msg.Role), Content: msg.Text}
		switch {
		case msg.Role == brain.RoleUser && len(msg.Image) > 0:
			item.Content = []map[string]any{
				{"type": "text", "text": msg.Text},
				{"type": "image_url", "image_url": map[string]string{
					"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(msg.Image)}},
			}
		case msg.Role == brain.RoleAssistant && len(msg.ToolCalls) > 0:
			if msg.Text == "" {
				item.Content = nil
			}
			for _, call := range msg.ToolCalls {
				arguments, _ := json.Marshal(call.Arguments)
				var wire openAIToolCall
				wire.ID, wire.Type = newID(call.ID, call.Name), "function"
				wire.Function.Name, wire.Function.Arguments = call.Name, string(arguments)
				item.ToolCalls = append(item.ToolCalls, wire)
			}
		case msg.Role == brain.RoleTool:
			item.ToolCallID = newID(msg.ToolCallID, msg.ToolName)
		}
		converted = append(converted, item)
	}
	return converted
}

func toOpenAITools(tools []brain.ToolSpec) []openAITool {
	converted := make([]openAITool, 0, len(tools))
	for _, tool := range tools {
		var wire openAITool
		wire.Type = "function"
		wire.Function.Name, wire.Function.Description, wire.Function.Parameters = tool.Name, tool.Description, tool.Parameters
		converted = append(converted, wire)
	}
	return converted
}

// ErrNoAPIKey 表示没有配置云端 API key。
var ErrNoAPIKey = errors.New("没有配置云端模型的 API key（HARO_CLOUD_LLM_API_KEY）")
