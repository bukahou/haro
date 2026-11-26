package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/llm/llmtest"
)

// 开着思考的一次回答：思考块（模型默认不返回思考的文字，只有签名）、加密的思考块、一句话、一次工具调用。
var thinkingThenToolEvents = []string{
	`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"enc-1"}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"我搜一下。"}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_07","name":"search_web","input":{}}}`,
	`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"东京 天气\"}"}}`,
	`{"type":"content_block_stop","index":3}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":120}}`,
	`{"type":"message_stop"}`,
}

// 开着思考时，回答收齐的那一段带着思考块（Replay）；带回去时思考块原样、在原来的位置，调用编号照样重写。
func TestAnthropicThinkingBlocksAreReplayed(t *testing.T) {
	server := llmtest.NewClaudeServer(t, llmtest.Script{Events: thinkingThenToolEvents})
	model := testClaudeModel(server, "low")
	reply, err := brain.CollectReply(t.Context(), model, brain.ChatRequest{Messages: []brain.ChatMessage{
		{Role: brain.RoleUser, Text: "东京天气"}}})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Replay == nil || reply.Replay.Provider != claudeReplayProvider {
		t.Fatalf("开着思考的回答应带着要原样带回的内容：%+v", reply.Replay)
	}
	if reply.Text != "我搜一下。" || len(reply.ToolCalls) != 1 || reply.ToolCalls[0].ID != "toolu_07" {
		t.Fatalf("回答 = %q %+v", reply.Text, reply.ToolCalls)
	}

	answer := brain.ChatMessage{Role: brain.RoleAssistant, Text: reply.Text, ToolCalls: reply.ToolCalls, Replay: reply.Replay}
	_, messages := toAnthropicMessages([]brain.ChatMessage{
		{Role: brain.RoleUser, Text: "东京天气"},
		answer,
		{Role: brain.RoleTool, ToolName: "search_web", ToolCallID: "toolu_07", Text: "晴"},
	})
	wire := marshalMessages(t, messages)
	want := `[{"content":[{"text":"东京天气","type":"text"}],"role":"user"},` +
		`{"content":[{"signature":"sig-1","thinking":"","type":"thinking"},{"data":"enc-1","type":"redacted_thinking"},` +
		`{"text":"我搜一下。","type":"text"},{"id":"call_0","input":{"query":"东京 天气"},"name":"search_web","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":[{"text":"晴","type":"text"}],"is_error":false,"tool_use_id":"call_0","type":"tool_result"}],"role":"user"}]`
	if wire != want {
		t.Errorf("思考块应原样、按原来的顺序带回：\n得到 %s\n应为 %s", wire, want)
	}
}

// 没开思考的回答不带 Replay（和原来一样）。
func TestAnthropicNoReplayWithoutThinking(t *testing.T) {
	server := llmtest.NewClaudeServer(t, llmtest.Script{Events: textThenToolEvents})
	reply, err := brain.CollectReply(t.Context(), testClaudeModel(server, "low"), brain.ChatRequest{Messages: []brain.ChatMessage{
		{Role: brain.RoleUser, Text: "东京天气怎么样"}}})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Replay != nil {
		t.Errorf("没有思考块时不该有 Replay：%s", reply.Replay.Data)
	}
}

// 带不回去的情况按没开思考的写法组装：brain 的消息和记下的对不上（文字改过、调用少了），记下的是别的适配层的，
// 或者解析不了。没跟着结果的调用照样去掉，思考块留着。
func TestAnthropicReplayFallsBackWhenItDoesNotMatch(t *testing.T) {
	recorded := &brain.ProviderReplay{Provider: claudeReplayProvider,
		Data: json.RawMessage(`[{"type":"thinking","signature":"sig-1"},{"type":"text","text":"我搜一下。"},{"type":"tool_use","tool_use_id":"t1"}]`)}
	call := brain.ToolCall{ID: "t1", Name: "search_web", Arguments: map[string]any{"query": "东京"}}
	result := brain.ChatMessage{Role: brain.RoleTool, ToolName: "search_web", ToolCallID: "t1", Text: "晴"}
	cases := map[string]brain.ChatMessage{
		"文字改过":   {Role: brain.RoleAssistant, Text: "我查一下。", ToolCalls: []brain.ToolCall{call}, Replay: recorded},
		"别的适配层":  {Role: brain.RoleAssistant, Text: "我搜一下。", ToolCalls: []brain.ToolCall{call}, Replay: &brain.ProviderReplay{Provider: "openai", Data: recorded.Data}},
		"解析不了":   {Role: brain.RoleAssistant, Text: "我搜一下。", ToolCalls: []brain.ToolCall{call}, Replay: &brain.ProviderReplay{Provider: claudeReplayProvider, Data: json.RawMessage(`{`)}},
		"调用对不上":  {Role: brain.RoleAssistant, Text: "我搜一下。", ToolCalls: []brain.ToolCall{{ID: "t2", Name: "search_web"}}, Replay: recorded},
		"多了一次调用": {Role: brain.RoleAssistant, Text: "我搜一下。", ToolCalls: []brain.ToolCall{call, {ID: "t3", Name: "read_pages"}}, Replay: recorded},
	}
	for name, answer := range cases {
		messages := []brain.ChatMessage{{Role: brain.RoleUser, Text: "东京天气"}, answer, result}
		if name == "调用对不上" {
			messages[2].ToolCallID = "t2"
		}
		_, converted := toAnthropicMessages(messages)
		if wire := marshalMessages(t, converted); strings.Contains(wire, `"type":"thinking"`) {
			t.Errorf("%s：不该带思考块：%s", name, wire)
		}
	}

	unanswered := brain.ChatMessage{Role: brain.RoleAssistant, Text: "我搜一下。", ToolCalls: []brain.ToolCall{call}, Replay: recorded}
	_, converted := toAnthropicMessages([]brain.ChatMessage{{Role: brain.RoleUser, Text: "东京天气"}, unanswered})
	wire := marshalMessages(t, converted)
	if !strings.Contains(wire, `"signature":"sig-1"`) || strings.Contains(wire, `"tool_use"`) {
		t.Errorf("没跟着结果的调用去掉、思考块留着：%s", wire)
	}
}

// 回答里有认不出的块（例如服务端工具）时不记 Replay：照原样排不回去。
func TestAnthropicNoReplayWithUnknownBlocks(t *testing.T) {
	events := append([]string{}, thinkingThenToolEvents[:4]...)
	events = append(events,
		`{"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":30}}`,
		`{"type":"message_stop"}`)
	server := llmtest.NewClaudeServer(t, llmtest.Script{Events: events})
	reply, err := brain.CollectReply(t.Context(), testClaudeModel(server, "low"), brain.ChatRequest{Messages: []brain.ChatMessage{
		{Role: brain.RoleUser, Text: "东京天气"}}})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Replay != nil {
		t.Errorf("有认不出的块时不该记 Replay：%s", reply.Replay.Data)
	}
}
