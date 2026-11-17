// Package llm 提供多模态对话模型的实现。目前只有 ollama（本机）。
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"unicode/utf8"

	"haro/internal/brain"
)

// OllamaChatModel 调用 ollama 的 /api/chat（流式）。
type OllamaChatModel struct {
	BaseURL     string
	Model       string // 例如 qwen3-vl:4b
	ContextSize int    // num_ctx
	KeepAlive   string // 模型在显存里保留多久，例如 "30m"
	Client      *http.Client
}

// NewOllamaChatModel 创建对话模型客户端。
func NewOllamaChatModel(baseURL, model string, contextSize int, keepAlive string, client *http.Client) *OllamaChatModel {
	return &OllamaChatModel{BaseURL: strings.TrimRight(baseURL, "/"), Model: model,
		ContextSize: contextSize, KeepAlive: keepAlive, Client: client}
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"`
	Images    []string         `json:"images,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"` // role=tool：这条是哪个工具的结果
}

type ollamaToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

type ollamaTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type ollamaChatRequest struct {
	Model     string          `json:"model"`
	Messages  []ollamaMessage `json:"messages"`
	Stream    bool            `json:"stream"`
	Tools     []ollamaTool    `json:"tools,omitempty"`
	Think     bool            `json:"think"` // 语音对话必须关闭深度思考，否则每句都要等很久
	KeepAlive string          `json:"keep_alive,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
}

// ollamaChatChunk 是流式响应的一行（NDJSON）；最后一行 done=true 并带统计。
type ollamaChatChunk struct {
	Message         ollamaMessage `json:"message"`
	Done            bool          `json:"done"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
	LoadDuration    int64         `json:"load_duration"` // 纳秒
	Error           string        `json:"error"`
}

// StreamReply 把消息列表发给模型，逐段产出可见文字与工具调用；思考内容只计字数，不产出。
func (m *OllamaChatModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		resp, err := m.postChat(ctx, toOllamaMessages(messages), toOllamaTools(tools), true)
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		defer resp.Body.Close()
		decoder := json.NewDecoder(resp.Body)
		thinkingChars := 0
		for {
			var chunk ollamaChatChunk
			if err := decoder.Decode(&chunk); err != nil {
				if errors.Is(err, io.EOF) {
					err = errors.New("ollama 流在 done 之前结束")
				}
				yield(brain.ReplyDelta{}, fmt.Errorf("读取 ollama 流: %w", err))
				return
			}
			if chunk.Error != "" {
				yield(brain.ReplyDelta{}, fmt.Errorf("ollama: %s", chunk.Error))
				return
			}
			thinkingChars += utf8.RuneCountInString(chunk.Message.Thinking)
			calls := fromOllamaToolCalls(chunk.Message.ToolCalls)
			if chunk.Done {
				yield(brain.ReplyDelta{Text: chunk.Message.Content, ToolCalls: calls, Usage: &brain.ReplyUsage{
					PromptTokens: chunk.PromptEvalCount, OutputTokens: chunk.EvalCount,
					ThinkingChars: thinkingChars, LoadMillis: chunk.LoadDuration / 1e6,
				}}, nil)
				return
			}
			if (chunk.Message.Content != "" || len(calls) > 0) &&
				!yield(brain.ReplyDelta{Text: chunk.Message.Content, ToolCalls: calls}, nil) {
				return // 调用方 break：关闭响应体即中止生成
			}
		}
	}
}

// Warmup 发一个空对话，让 ollama 把模型加载进显存。
func (m *OllamaChatModel) Warmup(ctx context.Context) error {
	resp, err := m.postChat(ctx, []ollamaMessage{}, nil, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// cameraFrameNote 是单独承载画面的那条用户消息的文字。
const cameraFrameNote = "这是摄像头当前画面。"

// toOllamaMessages 转换消息格式。带图片的用户消息拆成两条：先「画面」、再「问题」。
//
// 为什么拆：qwen3-vl 在「有对话历史 + 同一条消息里既有图片又有问题 + 给了工具」时会退化，
// 稳定地回答「你看到了什么？」、也不调用工具；图片单独一条后恢复正常（2026-11-15 实测：
// 一轮历史 3/3 退化 → 拆开后 3/3 正确调用；两轮历史同样）。
func toOllamaMessages(messages []brain.ChatMessage) []ollamaMessage {
	converted := make([]ollamaMessage, 0, len(messages)+1)
	for _, msg := range messages {
		item := ollamaMessage{Role: string(msg.Role), Content: msg.Text, ToolName: msg.ToolName}
		if len(msg.Image) > 0 {
			frame := base64.StdEncoding.EncodeToString(msg.Image)
			if msg.Role == brain.RoleUser {
				converted = append(converted, ollamaMessage{Role: "user", Content: cameraFrameNote, Images: []string{frame}})
			} else {
				item.Images = []string{frame}
			}
		}
		for _, call := range msg.ToolCalls {
			var wire ollamaToolCall
			wire.ID, wire.Function.Name, wire.Function.Arguments = call.ID, call.Name, call.Arguments
			item.ToolCalls = append(item.ToolCalls, wire)
		}
		converted = append(converted, item)
	}
	return converted
}

func toOllamaTools(tools []brain.ToolSpec) []ollamaTool {
	converted := make([]ollamaTool, 0, len(tools))
	for _, tool := range tools {
		var wire ollamaTool
		wire.Type = "function"
		wire.Function.Name, wire.Function.Description, wire.Function.Parameters = tool.Name, tool.Description, tool.Parameters
		converted = append(converted, wire)
	}
	return converted
}

func fromOllamaToolCalls(calls []ollamaToolCall) []brain.ToolCall {
	converted := make([]brain.ToolCall, 0, len(calls))
	for _, call := range calls {
		converted = append(converted, brain.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	return converted
}

// postChat 发出请求并检查状态码；调用方负责关闭响应体。
func (m *OllamaChatModel) postChat(ctx context.Context, messages []ollamaMessage, tools []ollamaTool, stream bool) (*http.Response, error) {
	payload, err := json.Marshal(ollamaChatRequest{
		Model: m.Model, Messages: messages, Tools: tools, Stream: stream, Think: false, KeepAlive: m.KeepAlive,
		Options: map[string]any{"num_ctx": m.ContextSize},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 ollama: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ollama 返回 %d: %s", resp.StatusCode, detail)
	}
	return resp, nil
}
