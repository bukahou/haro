//go:build integration

// 对真实 Claude API 的集成测试（默认不跑，CI 也不跑；每次运行都花钱：两个测试共 3 次小请求，不到 1 日元）：
//
//	HARO_ANTHROPIC_API_KEY=… go test -tags integration -run AnthropicLive -v -count=1 ./internal/llm/
//
// 验证：工具调用来回、提示词缓存命中、「这一轮不给工具但历史里有工具往来」的请求被接受；打印首字时间和用量。
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

func TestAnthropicLive(t *testing.T) {
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY")
	}
	modelName := os.Getenv("HARO_TEST_CLAUDE_MODEL") // 不读 HARO_CLOUD_LLM_MODEL：开发机上它可能还指向别家的模型
	if modelName == "" {
		modelName = "claude-haiku-5-5"
	}
	model := NewAnthropicChatModel(AnthropicOptions{API: NewClaudeAPI(key, "", nil), Model: modelName, MaxTokens: 300, Effort: "low"})

	// 系统提示词要超过最小可缓存长度，才能看到缓存命中（haro 实际的提示词约 6 千 token）
	var system strings.Builder
	system.WriteString("你是家用语音助手 haro，用中文简短回答，一两句话。需要知道时间时调用 get_current_time。\n")
	for i := 1; i <= 120; i++ {
		fmt.Fprintf(&system, "备用规则 %d：回答要口语化，不用列表、不用表情符号，数字按中文习惯读出来，不确定时直接说不确定。\n", i)
	}
	tools := []brain.ToolSpec{{Name: "get_current_time", Description: "查现在的日期和时间",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
	history := []brain.ChatMessage{{Role: brain.RoleSystem, Text: system.String()}, {Role: brain.RoleUser, Text: "现在几点了？"}}

	// 1. 应调用工具；写入缓存
	text, calls, usage := liveRound(t, model, history, tools, "第 1 次（应调用工具）")
	if len(calls) != 1 || calls[0].Name != "get_current_time" {
		t.Fatalf("应调用 get_current_time，实际文字 %q、调用 %+v", text, calls)
	}
	if usage.CacheWriteTokens == 0 && usage.CacheReadTokens == 0 {
		t.Errorf("提示词缓存没有生效（写 0、读 0）：%+v", usage)
	}

	// 2. 交回工具结果：应给出回答；工具定义 + 系统提示词应命中缓存
	history = append(history,
		brain.ChatMessage{Role: brain.RoleAssistant, Text: text, ToolCalls: calls},
		brain.ChatMessage{Role: brain.RoleTool, ToolName: calls[0].Name, ToolCallID: calls[0].ID, Text: `{"now":"2026-11-18 14:05"}`})
	answer, _, usage := liveRound(t, model, history, tools, "第 2 次（交回工具结果）")
	if answer == "" {
		t.Error("交回工具结果后应有回答")
	}
	if usage.CacheReadTokens == 0 {
		t.Errorf("第 2 次应命中缓存：%+v", usage)
	}
}

// 这一轮不给工具（最后一轮逼模型直接回答），历史里有工具往来：补上的工具定义 + tool_choice none 要被接受（1 次小请求）。
func TestAnthropicLiveNoToolsRound(t *testing.T) {
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY")
	}
	modelName := os.Getenv("HARO_TEST_CLAUDE_MODEL")
	if modelName == "" {
		modelName = "claude-haiku-5-5"
	}
	model := NewAnthropicChatModel(AnthropicOptions{API: NewClaudeAPI(key, "", nil), Model: modelName, MaxTokens: 200, Effort: "low"})
	history := []brain.ChatMessage{
		{Role: brain.RoleSystem, Text: "你是家用语音助手 haro，用中文一句话回答。"},
		{Role: brain.RoleUser, Text: "现在几点了？"},
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "call-from-local-model:0", Name: "get_current_time"}}},
		{Role: brain.RoleTool, ToolName: "get_current_time", ToolCallID: "call-from-local-model:0", Text: `{"now":"2026-11-18 14:05"}`},
	}
	answer, calls, _ := liveRound(t, model, history, nil, "不给工具")
	if answer == "" || len(calls) > 0 {
		t.Errorf("不给工具时应直接回答：文字 %q、调用 %+v", answer, calls)
	}
}

func liveRound(t *testing.T, model *AnthropicChatModel, messages []brain.ChatMessage, tools []brain.ToolSpec,
	label string) (string, []brain.ToolCall, brain.ReplyUsage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started := time.Now()
	var firstText time.Duration
	var text strings.Builder
	var calls []brain.ToolCall
	var usage brain.ReplyUsage
	for delta, err := range model.StreamReply(ctx, messages, tools) {
		if err != nil {
			t.Fatalf("%s：%v", label, err)
		}
		if delta.Text != "" && firstText == 0 {
			firstText = time.Since(started)
		}
		text.WriteString(delta.Text)
		calls = append(calls, delta.ToolCalls...)
		if delta.Usage != nil {
			usage = *delta.Usage
		}
	}
	t.Logf("%s：总耗时 %v，首字 %v，文字 %q，调用 %d 个；输入 %d（缓存读 %d、写 %d），输出 %d", label,
		time.Since(started).Round(time.Millisecond), firstText.Round(time.Millisecond), text.String(), len(calls),
		usage.PromptTokens, usage.CacheReadTokens, usage.CacheWriteTokens, usage.OutputTokens)
	return text.String(), calls, usage
}
