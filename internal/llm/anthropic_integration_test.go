//go:build integration

// 对真实 Claude API 的集成测试（默认不跑，CI 也不跑；每次运行都花钱：两个测试共 3 次小请求，不到 1 日元）：
//
//	HARO_ANTHROPIC_API_KEY=… go test -tags integration -run AnthropicLive -v -count=1 ./internal/llm/
//
// 验证：工具调用来回、提示词缓存命中、「这一轮不给工具但历史里有工具往来」的请求被接受；打印首字时间和用量。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

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

// 更强的模型（默认 Sonnet 5.5，HARO_TEST_STRONG_MODEL 可改）：关思考要用 between_tools；工具来回两三次
// （两次工具调用之间它会返回思考块，haro 带不回、丢掉）后请求仍被接受；不给工具的那一轮被接受；带图片的请求能回答。
// 约 5 次小请求，不到 5 日元。
func TestAnthropicLiveStrongModel(t *testing.T) {
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY")
	}
	modelName := os.Getenv("HARO_TEST_STRONG_MODEL")
	if modelName == "" {
		modelName = "claude-sonnet-5-5"
	}
	model := NewAnthropicChatModel(AnthropicOptions{API: NewClaudeAPI(key, "", nil), Model: modelName, MaxTokens: 400,
		Effort: "low", Thinking: ThinkingBetweenTools})
	tools := []brain.ToolSpec{
		{Name: "get_current_time", Description: "查现在的日期和时间", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: "get_weather", Description: "查一个地方现在的天气", Parameters: json.RawMessage(
			`{"type":"object","properties":{"place":{"type":"string"}},"required":["place"]}`)},
	}
	history := []brain.ChatMessage{
		{Role: brain.RoleSystem, Text: "你是家用语音助手 haro，用中文一句话回答。先用 get_current_time 查时间，再用 get_weather 查天气，每次只调用一个工具，都查完再回答。"},
		{Role: brain.RoleUser, Text: "现在几点？东京天气怎么样？"},
	}
	results := map[string]string{"get_current_time": `{"now":"2026-11-18 16:30"}`, "get_weather": `{"weather":"晴","temperature_c":18}`}
	answer := ""
	for round := 1; round <= 4 && answer == ""; round++ {
		text, calls, _ := liveRound(t, model, history, tools, fmt.Sprintf("第 %d 次", round))
		if len(calls) == 0 {
			answer = text
			break
		}
		history = append(history, brain.ChatMessage{Role: brain.RoleAssistant, Text: text, ToolCalls: calls})
		for _, call := range calls {
			history = append(history, brain.ChatMessage{Role: brain.RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: results[call.Name]})
		}
	}
	if answer == "" {
		t.Fatal("工具来回之后应给出回答")
	}

	// 不给工具的那一轮（历史里有工具往来 ⇒ 补工具定义 + tool_choice none）
	history = append(history, brain.ChatMessage{Role: brain.RoleAssistant, Text: answer}, brain.ChatMessage{Role: brain.RoleUser, Text: "谢谢，再说一遍时间就好"})
	if text, calls, _ := liveRound(t, model, history, nil, "不给工具"); text == "" || len(calls) > 0 {
		t.Errorf("不给工具时应直接回答：%q %+v", text, calls)
	}

	// 带图片：纯红的方块
	picture := image.NewRGBA(image.Rect(0, 0, 64, 64))
	draw.Draw(picture, picture.Bounds(), &image.Uniform{C: color.RGBA{R: 220, A: 255}}, image.Point{}, draw.Src)
	var frame bytes.Buffer
	if err := jpeg.Encode(&frame, picture, nil); err != nil {
		t.Fatal(err)
	}
	look := []brain.ChatMessage{{Role: brain.RoleSystem, Text: "只用 report_color 工具回答，不要说话。"},
		{Role: brain.RoleUser, Text: "这张图是什么颜色？"}, {Role: brain.RoleUser, Text: "这是摄像头当前画面。", Image: frame.Bytes()}}
	_, calls, _ := liveRound(t, model, look, []brain.ToolSpec{colorReportTool(t)}, "带图片")
	if len(calls) != 1 || calls[0].Arguments["color"] != "red" {
		t.Errorf("应经 report_color 报告红色：%+v", calls)
	}
}

// colorReportTool 让模型用固定的几个值报告颜色（看图的结论是结构化的，不从文字里找词）。
func colorReportTool(t *testing.T) brain.ToolSpec {
	t.Helper()
	type colorReport struct {
		Color string `json:"color" jsonschema:"画面的主要颜色"`
	}
	schema, err := jsonschema.For[colorReport](nil)
	if err != nil {
		t.Fatal(err)
	}
	schema.Properties["color"].Enum = []any{"red", "green", "blue", "white", "black", "other"}
	parameters, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	return brain.ToolSpec{Name: "report_color", Description: "报告画面的主要颜色。", Parameters: parameters}
}
