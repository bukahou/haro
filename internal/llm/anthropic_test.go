package llm

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/llm/llmtest"
)

func testClaudeModel(server *llmtest.Server, effort string) *AnthropicChatModel {
	return NewAnthropicChatModel(AnthropicOptions{API: claudeAPIForTest("test-key", server.URL, server.Client()),
		Model: "claude-haiku-5-5", MaxTokens: 800, Effort: effort})
}

// 一次「先说一句、再调用工具」的回答：缓存读 5000、写 300、没命中 12，输出 42。
var textThenToolEvents = []string{
	`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"cache_creation_input_tokens":300,"cache_read_input_tokens":5000,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我查一下"}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"天气。"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"东京\"}"}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`,
	`{"type":"message_stop"}`,
}

func TestAnthropicStreamsTextToolCallsAndCacheUsage(t *testing.T) {
	server := llmtest.NewClaudeServer(t, llmtest.Script{Events: textThenToolEvents})
	model := testClaudeModel(server, "low")
	var pieces []string
	var calls []brain.ToolCall
	var usage *brain.ReplyUsage
	for delta, err := range model.StreamReply(t.Context(), brain.ChatRequest{Messages: []brain.ChatMessage{
		{Role: brain.RoleSystem, Text: "你是 haro。"},
		{Role: brain.RoleUser, Text: "东京天气怎么样"},
	}, Tools: []brain.ToolSpec{{Name: "get_weather", Description: "查天气",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`)}}}) {
		if err != nil {
			t.Fatal(err)
		}
		if delta.Text != "" {
			pieces = append(pieces, delta.Text)
		}
		calls = append(calls, delta.ToolCalls...)
		if delta.Usage != nil {
			usage = delta.Usage
		}
	}
	if got := strings.Join(pieces, "|"); got != "我查一下|天气。" {
		t.Errorf("文字应该逐段到达：%q", got)
	}
	if len(calls) != 1 || calls[0].ID != "toolu_01" || calls[0].Name != "get_weather" || calls[0].Arguments["city"] != "东京" {
		t.Errorf("工具调用 = %+v", calls)
	}
	want := brain.ReplyUsage{PromptTokens: 5312, OutputTokens: 42, CacheReadTokens: 5000, CacheWriteTokens: 300}
	if usage == nil || *usage != want {
		t.Errorf("用量 = %+v，应为 %+v（输入总数 = 没命中 + 缓存读 + 缓存写）", usage, want)
	}

	body := server.Request(0)
	if body["model"] != "claude-haiku-5-5" || body["max_tokens"] != float64(800) || body["stream"] != true {
		t.Errorf("模型 / 输出上限 / 流式不对：%v %v %v", body["model"], body["max_tokens"], body["stream"])
	}
	if thinking, _ := body["thinking"].(map[string]any); thinking["type"] != "disabled" {
		t.Errorf("思考应该关掉：%v", body["thinking"])
	}
	if output, _ := body["output_config"].(map[string]any); output["effort"] != "low" {
		t.Errorf("回答力度应为 low：%v", body["output_config"])
	}
	if _, set := body["cache_control"]; set {
		t.Errorf("不该用请求级自动缓存（断点落在每轮都变的「现在的情况」后面）：%v", body["cache_control"])
	}
	system, _ := body["system"].([]any)
	if len(system) != 1 {
		t.Fatalf("系统提示词应在顶层 system：%v", body["system"])
	}
	if block := system[0].(map[string]any); block["text"] != "你是 haro。" || block["cache_control"] == nil {
		t.Errorf("系统提示词末尾应有缓存断点：%v", block)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("工具定义 = %v", body["tools"])
	}
	tool := tools[0].(map[string]any)
	schema, _ := tool["input_schema"].(map[string]any)
	if tool["name"] != "get_weather" || tool["description"] != "查天气" || schema["type"] != "object" ||
		schema["additionalProperties"] != false || fmt.Sprint(schema["required"]) != "[city]" || schema["properties"] == nil {
		t.Errorf("工具定义转换不对：%v", tool)
	}
	if _, set := body["tool_choice"]; set {
		t.Errorf("给了工具时不该限制调用：%v", body["tool_choice"])
	}
	header := server.Header(0)
	if header.Get("X-Api-Key") != "test-key" && header.Get("Authorization") != "Bearer test-key" {
		t.Errorf("没带上配置的 key：%v", header)
	}
}

// 看画面那一轮：工具结果和画面并进同一条用户消息（工具结果在前），工具调用 ID 重写成 call_N 且前后一致。
func TestAnthropicMessagesForCameraTurn(t *testing.T) {
	frame := []byte{0xff, 0xd8, 0xff, 0xd9}
	system, messages := toAnthropicMessages([]brain.ChatMessage{
		{Role: brain.RoleSystem, Text: "你是 haro。"},
		{Role: brain.RoleUser, Text: "桌上有什么"},
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "functions.look_at_camera:0", Name: "look_at_camera"}}},
		{Role: brain.RoleTool, ToolName: "look_at_camera", ToolCallID: "functions.look_at_camera:0", Text: "已拿到画面，见下一条消息。"},
		{Role: brain.RoleUser, Text: "（摄像头画面）", Image: frame},
	})
	if system != "你是 haro。" {
		t.Errorf("system = %q", system)
	}
	wire := marshalMessages(t, messages)
	want := `[{"content":[{"text":"桌上有什么","type":"text"}],"role":"user"},` +
		`{"content":[{"id":"call_0","input":{},"name":"look_at_camera","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":[{"text":"已拿到画面，见下一条消息。","type":"text"}],"is_error":false,"tool_use_id":"call_0","type":"tool_result"},` +
		`{"text":"（摄像头画面）","type":"text"},` +
		`{"source":{"data":"` + base64.StdEncoding.EncodeToString(frame) + `","media_type":"image/jpeg","type":"base64"},"type":"image"}],"role":"user"}]`
	if wire != want {
		t.Errorf("消息转换不对：\n得到 %s\n应为 %s", wire, want)
	}
}

// 配不上对的工具往来（Claude 对此直接 400）：没有结果的调用去掉，没有调用的结果改成普通文字。
func TestAnthropicMessagesDropUnpairedToolBlocks(t *testing.T) {
	_, messages := toAnthropicMessages([]brain.ChatMessage{
		{Role: brain.RoleUser, Text: "记一下，再查天气"},
		{Role: brain.RoleAssistant, Text: "好。", ToolCalls: []brain.ToolCall{
			{ID: "a", Name: "save_note", Arguments: map[string]any{"text": "买牛奶"}},
			{ID: "b", Name: "get_weather"}, // 没有结果（例如中途被打断）
		}},
		{Role: brain.RoleTool, ToolName: "save_note", ToolCallID: "a", Text: "已记下"},
		{Role: brain.RoleAssistant, Text: "记下了。"},
		{Role: brain.RoleTool, ToolName: "get_weather", ToolCallID: "c", Text: "晴"}, // 前面没有对应的调用
	})
	wire := marshalMessages(t, messages)
	if strings.Contains(wire, `"name":"get_weather"`) {
		t.Errorf("没有结果的调用应该去掉：%s", wire)
	}
	if !strings.Contains(wire, `"id":"call_0","input":{"text":"买牛奶"},"name":"save_note"`) ||
		!strings.Contains(wire, `"tool_use_id":"call_0"`) {
		t.Errorf("配上对的调用和结果应保留且 ID 一致：%s", wire)
	}
	if !strings.Contains(wire, `（工具 get_weather 的结果）晴`) {
		t.Errorf("没有调用的结果应改成文字：%s", wire)
	}
}

// 这一轮不给工具（最后一轮逼模型直接回答）但历史里有工具往来：按历史补上工具定义，并声明这一轮不许调用。
func TestAnthropicNoToolsRoundKeepsHistoryValid(t *testing.T) {
	server := llmtest.NewClaudeServer(t, llmtest.Script{Events: []string{`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"晴天。"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`}})
	model := testClaudeModel(server, "")
	for _, err := range model.StreamReply(t.Context(), brain.ChatRequest{Messages: []brain.ChatMessage{
		{Role: brain.RoleUser, Text: "天气"},
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "x", Name: "get_weather"}}},
		{Role: brain.RoleTool, ToolName: "get_weather", ToolCallID: "x", Text: "晴"},
	}}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	body := server.Request(0)
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("应按历史补上工具定义：%v", body["tools"])
	}
	// 2026-11-18 真实请求查出：补上的工具没有参数定义时 input_schema 被省掉，Claude 回 400（input_schema: Field required）
	if schema, _ := tools[0].(map[string]any)["input_schema"].(map[string]any); schema["type"] != "object" {
		t.Errorf("补上的工具也要有 input_schema：%v", tools[0])
	}
	if choice, _ := body["tool_choice"].(map[string]any); choice["type"] != "none" {
		t.Errorf("这一轮应声明不许调用工具：%v", body["tool_choice"])
	}
	if _, set := body["output_config"]; set {
		t.Errorf("没配力度时不该发 output_config：%v", body["output_config"])
	}
}

// 拒答：当作失败（退路接手），但用量照样带出来记账；没有退路时能念给主人。
func TestAnthropicRefusalIsAnErrorWithUsage(t *testing.T) {
	server := llmtest.NewClaudeServer(t, llmtest.Script{Events: []string{`{"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":50,"output_tokens":1}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"test"}},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`}})
	model := testClaudeModel(server, "low")
	var refusal *RefusalError
	var usage *brain.ReplyUsage
	for delta, err := range model.StreamReply(t.Context(), brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "?"}}}) {
		if delta.Usage != nil {
			usage = delta.Usage
		}
		if err != nil && !errors.As(err, &refusal) {
			t.Fatalf("应是拒答错误：%v", err)
		}
	}
	if refusal == nil || refusal.Category != "cyber" {
		t.Fatalf("拒答没报出来：%+v", refusal)
	}
	var spoken brain.SpokenError
	if !errors.As(error(refusal), &spoken) {
		t.Error("拒答应能念给主人")
	} else if key, _ := spoken.Phrase(); key != brain.PhraseRefused {
		t.Errorf("拒答念的那句：%s", key)
	}
	if usage == nil || usage.PromptTokens != 50 || usage.OutputTokens != 3 {
		t.Errorf("拒答也要带出用量记账：%+v", usage)
	}
}

// 结束标志要齐（message_delta 带的停下原因 + message_stop）：缺哪个都按流没收尾报错（算连不上），用量标不完整；
// 上下文满了（model_context_window_exceeded）和到了输出上限一样按截断交出，不交出写到一半的工具调用。
func TestAnthropicStreamEnding(t *testing.T) {
	question := brain.ChatRequest{Messages: contractQuestion, Tools: []brain.ToolSpec{contractWeather}}
	unfinished := claudeEvents([]string{"从前"}, nil, contractUnfinished)
	delta := func(reason string) string {
		return fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":20}}`, reason)
	}
	for _, c := range []struct {
		name   string
		events []string
	}{
		{"只有停下的原因、没有 message_stop", append(slices.Clone(unfinished), delta("end_turn"))},
		{"只有 message_stop、没有停下的原因", append(slices.Clone(unfinished), `{"type":"message_stop"}`)},
	} {
		server := llmtest.NewClaudeServer(t, llmtest.Script{Events: c.events})
		reply := collectContract(t.Context(), testClaudeModel(server, ""), question, nil)
		if !errors.Is(reply.err, brain.ErrModelUnavailable) || reply.text() != "从前" || reply.usage == nil || !reply.usage.Incomplete {
			t.Errorf("%s：应按没收尾报错：%+v", c.name, reply)
		}
	}

	full := claudeEvents([]string{"从前有座山，"}, contractCall, contractMaxTokens)
	full[len(full)-2] = delta(string(anthropic.StopReasonModelContextWindowExceeded))
	server := llmtest.NewClaudeServer(t, llmtest.Script{Events: full})
	reply := collectContract(t.Context(), testClaudeModel(server, ""), question, nil)
	if reply.err != nil || reply.stop != brain.StopMaxTokens || len(reply.calls) != 0 || reply.usage == nil || reply.usage.Incomplete {
		t.Errorf("上下文满了应按截断交出：%+v", reply)
	}
}

// 服务端过载：MaxRetries 为 0 时只请求一次就报错（聊天时由退路接手，不在 SDK 里多等），且什么都没产出。
func TestAnthropicServerErrorFailsFastWithoutRetry(t *testing.T) {
	server := llmtest.NewClaudeServer(t, llmtest.Script{Status: 529})
	model := testClaudeModel(server, "low")
	produced := false
	var failure error
	for delta, err := range model.StreamReply(t.Context(), brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "你好"}}}) {
		if delta.Text != "" || len(delta.ToolCalls) > 0 {
			produced = true
		}
		if err != nil {
			failure = err
		}
	}
	if failure == nil || produced {
		t.Fatalf("过载时应在产出任何内容之前报错：err=%v produced=%v", failure, produced)
	}
	if got := server.Count(); got != 1 {
		t.Errorf("MaxRetries=0 时只应请求 1 次，实际 %d 次", got)
	}
}

func marshalMessages(t *testing.T, messages any) string {
	t.Helper()
	wire, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	// 统一成按键排序的形式再比较
	var generic any
	if err := json.Unmarshal(wire, &generic); err != nil {
		t.Fatal(err)
	}
	sorted, _ := json.Marshal(generic)
	return string(sorted)
}

// Claude 的回答力度（空 = 模型默认）按搭配的思考写法查：三档哪种写法都行；xhigh、max 只在 adaptive 时可用；别的报错。
func TestValidateClaudeValues(t *testing.T) {
	for _, thinking := range []string{ThinkingDisabled, ThinkingBetweenTools, ThinkingAdaptive} {
		for _, effort := range []string{"", "low", "medium", "high"} {
			if err := ValidateClaudeEffort(effort, thinking); err != nil {
				t.Errorf("力度 %q（思考 %s）应通过：%v", effort, thinking, err)
			}
		}
	}
	for _, effort := range []string{"xhigh", "max"} {
		if err := ValidateClaudeEffort(effort, ThinkingAdaptive); err != nil {
			t.Errorf("思考是 adaptive 时力度 %q 应通过：%v", effort, err)
		}
		for _, thinking := range []string{ThinkingDisabled, ThinkingBetweenTools} {
			if err := ValidateClaudeEffort(effort, thinking); err == nil {
				t.Errorf("思考是 %s 时力度 %q 应报错", thinking, effort)
			}
		}
	}
	if err := ValidateClaudeEffort("Low", ThinkingAdaptive); err == nil {
		t.Error("不认识的力度应报错")
	}
	for _, thinking := range []string{ThinkingDisabled, ThinkingBetweenTools, ThinkingAdaptive} {
		if err := ValidateClaudeThinking(thinking); err != nil {
			t.Errorf("思考 %q 应通过：%v", thinking, err)
		}
	}
	if err := ValidateClaudeThinking(""); err == nil {
		t.Error("思考的写法要写明（空的不认）")
	}
}

// claudeAPIForTest 建测试用的 Claude 客户端（测试都给了凭证：建不起来是测试本身写错了）。
func claudeAPIForTest(apiKey, baseURL string, client *http.Client) *ClaudeAPI {
	api, err := NewClaudeAPI(apiKey, baseURL, client)
	if err != nil {
		panic(err)
	}
	return api
}

// CacheTTL 设成 1 小时时，三个缓存断点都带 ttl: 1h（不混用）；用量里写进 1 小时缓存的单独记（CacheWriteLongTokens）。
// 默认（空）不带 ttl，按 API 的 5 分钟（阳性对照在 TestAnthropicStreamsTextToolCallsAndCacheUsage：断点照样有）。
func TestAnthropicLongCacheTTL(t *testing.T) {
	events := slices.Clone(textThenToolEvents)
	events[0] = strings.Replace(events[0], `"cache_creation_input_tokens":300,`,
		`"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_1h_input_tokens":300,"ephemeral_5m_input_tokens":0},`, 1)
	for _, c := range []struct {
		ttl, want string
		long      int
	}{{CacheTTL1h, "1h", 300}, {"", "", 300}} {
		server := llmtest.NewClaudeServer(t, llmtest.Script{Events: events})
		model := NewAnthropicChatModel(AnthropicOptions{API: claudeAPIForTest("test-key", server.URL, server.Client()),
			Model: "claude-haiku-5-5", MaxTokens: 800, CacheTTL: c.ttl})
		messages := []brain.ChatMessage{{Role: brain.RoleSystem, Text: "你是 haro。"}, {Role: brain.RoleUser, Text: "上一轮"},
			{Role: brain.RoleAssistant, Text: "上一轮的回答"}, {Role: brain.RoleUser, Text: "这一轮"}}
		reply, err := brain.CollectReply(t.Context(), model, brain.ChatRequest{Messages: messages})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Usage.CacheWriteLongTokens != c.long || reply.Usage.CacheWriteTokens != 300 {
			t.Errorf("ttl %q：1 小时缓存的写入应单独记：%+v", c.ttl, reply.Usage)
		}
		body := server.Request(0)
		system := body["system"].([]any)[0].(map[string]any)["cache_control"].(map[string]any)
		previous := body["messages"].([]any)[1].(map[string]any)["content"].([]any)
		turn := previous[len(previous)-1].(map[string]any)["cache_control"].(map[string]any)
		for name, breakpoint := range map[string]map[string]any{"系统提示词": system, "上一轮结束处": turn} {
			if got, _ := breakpoint["ttl"].(string); got != c.want || breakpoint["type"] != "ephemeral" {
				t.Errorf("ttl %q：%s的断点 = %v，ttl 应为 %q", c.ttl, name, breakpoint, c.want)
			}
		}
	}
}
