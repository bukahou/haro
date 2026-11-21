package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/llm/llmtest"
)

// 模型适配的契约：Claude、OpenAI 兼容（云端）、Ollama（OpenAI 兼容端点）对着各自的假服务端跑同一组场景。
// 每家只提供「这个场景下服务端推什么」和「请求体里怎么看工具、有没有声明不许调用」，断言在场景里只写一次。

// contractStop 是脚本里模型停下的原因。
type contractStop int

const (
	contractDone      contractStop = iota // 说完了
	contractToolUse                       // 停下来等工具结果
	contractMaxTokens                     // 到了输出上限（工具调用的参数写到一半）
)

type contractVendor struct {
	name      string
	newServer func(t testing.TB, scripts ...llmtest.Script) *llmtest.Server
	newModel  func(server *llmtest.Server) brain.ChatModel
	// events 是一次回答要推的事件：逐段说 texts，再（call 不为空时）调用一个工具，最后按 stop 停下；用量是输入 100、输出 20。
	// 第 firstText 个事件（从 0 数）是第一段文字。
	events    func(texts []string, call *brain.ToolCall, stop contractStop) []string
	firstText int
	tools     func(body map[string]any) []any // 请求体里的工具定义
	toolName  func(tool any) string
	forbids   func(body map[string]any) bool // 请求体里声明了这一轮不许调用工具
	// omitsForbiddenTools：服务端不认「不许调用」的选项，这一轮干脆不带工具（Ollama）
	omitsForbiddenTools bool
}

var contractVendors = []contractVendor{
	{name: "claude", newServer: llmtest.NewClaudeServer, events: claudeEvents, firstText: 2,
		newModel: func(server *llmtest.Server) brain.ChatModel {
			return NewAnthropicChatModel(AnthropicOptions{API: NewClaudeAPI("test-key", server.URL, server.Client()),
				Model: "claude-haiku-5-5", MaxTokens: 800})
		},
		tools:    func(body map[string]any) []any { tools, _ := body["tools"].([]any); return tools },
		toolName: func(tool any) string { name, _ := tool.(map[string]any)["name"].(string); return name },
		forbids: func(body map[string]any) bool {
			choice, _ := body["tool_choice"].(map[string]any)
			return choice["type"] == "none"
		}},
	{name: "openai", newServer: llmtest.NewOpenAIServer, events: openAIEvents, firstText: 0,
		newModel: func(server *llmtest.Server) brain.ChatModel {
			return NewOpenAIChatModel(OpenAIOptions{BaseURL: server.URL, Model: "gpt-test", APIKey: "k", MaxTokens: 800,
				HTTPClient: server.Client()})
		},
		tools: openAITools, toolName: openAIToolName, forbids: openAIForbids},
	{name: "ollama", newServer: llmtest.NewOpenAIServer, events: openAIEvents, firstText: 0, omitsForbiddenTools: true,
		newModel: func(server *llmtest.Server) brain.ChatModel {
			return NewOllamaChatModel(server.URL, "qwen3-vl:8b-instruct", 800, OpenAIOptions{HTTPClient: server.Client()})
		},
		tools: openAITools, toolName: openAIToolName, forbids: openAIForbids},
}

func claudeEvents(texts []string, call *brain.ToolCall, stop contractStop) []string {
	events := []string{`{"type":"message_start","message":{"id":"msg_c","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`}
	for _, text := range texts {
		events = append(events, fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text))
	}
	events = append(events, `{"type":"content_block_stop","index":0}`)
	if call != nil {
		events = append(events, fmt.Sprintf(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, call.ID, call.Name),
			fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":%q}}`, contractArguments(call, stop)),
			`{"type":"content_block_stop","index":1}`)
	}
	reason := map[contractStop]string{contractDone: "end_turn", contractToolUse: "tool_use", contractMaxTokens: "max_tokens"}[stop]
	return append(events, fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":20}}`, reason),
		`{"type":"message_stop"}`)
}

func openAIEvents(texts []string, call *brain.ToolCall, stop contractStop) []string {
	var events []string
	for _, text := range texts {
		events = append(events, fmt.Sprintf(`{"id":"cc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q}}]}`, text))
	}
	if call != nil {
		events = append(events, fmt.Sprintf(`{"id":"cc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`,
			call.ID, call.Name, contractArguments(call, stop)))
	}
	reason := map[contractStop]string{contractDone: "stop", contractToolUse: "tool_calls", contractMaxTokens: "length"}[stop]
	return append(events, fmt.Sprintf(`{"id":"cc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, reason),
		`{"id":"cc","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`)
}

// contractArguments 是工具调用的参数 JSON；到了输出上限时只写了一半。
func contractArguments(call *brain.ToolCall, stop contractStop) string {
	arguments, _ := json.Marshal(call.Arguments)
	if stop == contractMaxTokens {
		return string(arguments[:len(arguments)/2])
	}
	return string(arguments)
}

func openAITools(body map[string]any) []any { tools, _ := body["tools"].([]any); return tools }
func openAIToolName(tool any) string {
	function, _ := tool.(map[string]any)["function"].(map[string]any)
	name, _ := function["name"].(string)
	return name
}
func openAIForbids(body map[string]any) bool { return body["tool_choice"] == "none" }

var (
	contractWeather = brain.ToolSpec{Name: "get_weather", Description: "查天气",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}
	contractCall     = &brain.ToolCall{ID: "call_c1", Name: "get_weather", Arguments: map[string]any{"city": "东京"}}
	contractQuestion = []brain.ChatMessage{{Role: brain.RoleUser, Text: "东京天气怎么样"}}
)

// contractReply 是收齐的一次回答（契约里要看的每一项）。
type contractReply struct {
	texts []string // 每段文字（看是不是逐段到达）
	calls []brain.ToolCall
	usage *brain.ReplyUsage
	stop  brain.StopReason
	err   error
}

func (r contractReply) text() string { return strings.Join(r.texts, "") }

// collectContract 收齐一次回答；onText 在每段文字到达时调用（测打断用）。
func collectContract(ctx context.Context, model brain.ChatModel, request brain.ChatRequest, onText func()) contractReply {
	var reply contractReply
	for delta, err := range model.StreamReply(ctx, request) {
		if delta.Usage != nil {
			reply.usage = delta.Usage
		}
		if err != nil {
			reply.err = err
			return reply
		}
		if delta.Text != "" {
			reply.texts = append(reply.texts, delta.Text)
			if onText != nil {
				onText()
			}
		}
		reply.calls = append(reply.calls, delta.ToolCalls...)
		reply.stop = delta.Stop
	}
	return reply
}

func forEachVendor(t *testing.T, run func(t *testing.T, vendor contractVendor)) {
	for _, vendor := range contractVendors {
		t.Run(vendor.name, func(t *testing.T) { run(t, vendor) })
	}
}

// 文字逐段到达；说完时最后一段带用量，停下的原因是说完了。
func TestContractStreamsTextWithUsage(t *testing.T) {
	forEachVendor(t, func(t *testing.T, vendor contractVendor) {
		server := vendor.newServer(t, llmtest.Script{Events: vendor.events([]string{"你好，", "我在。"}, nil, contractDone)})
		reply := collectContract(t.Context(), vendor.newModel(server), brain.ChatRequest{Messages: contractQuestion}, nil)
		if reply.err != nil || len(reply.texts) != 2 || reply.text() != "你好，我在。" || len(reply.calls) != 0 || reply.stop != brain.StopCompleted {
			t.Errorf("逐段说完：%+v", reply)
		}
		if reply.usage == nil || reply.usage.PromptTokens != 100 || reply.usage.OutputTokens != 20 || reply.usage.Incomplete {
			t.Errorf("最后应带完整的用量：%+v", reply.usage)
		}
	})
}

// 先说一句再调用工具：工具调用（名字、参数）在流结束时交出，用量照带。
func TestContractToolCall(t *testing.T) {
	forEachVendor(t, func(t *testing.T, vendor contractVendor) {
		server := vendor.newServer(t, llmtest.Script{Events: vendor.events([]string{"我查一下。"}, contractCall, contractToolUse)})
		reply := collectContract(t.Context(), vendor.newModel(server),
			brain.ChatRequest{Messages: contractQuestion, Tools: []brain.ToolSpec{contractWeather}}, nil)
		if reply.err != nil || reply.text() != "我查一下。" || len(reply.calls) != 1 || reply.calls[0].Name != "get_weather" ||
			reply.calls[0].Arguments["city"] != "东京" || reply.usage == nil || reply.stop != brain.StopCompleted {
			t.Errorf("工具调用：%+v", reply)
		}
	})
}

// 输出到了上限：明确交出「被截断」，不当正常结束；写到一半的工具调用不交出；用量照带。
func TestContractTruncation(t *testing.T) {
	forEachVendor(t, func(t *testing.T, vendor contractVendor) {
		server := vendor.newServer(t, llmtest.Script{Events: vendor.events([]string{"从前有座山，"}, contractCall, contractMaxTokens)})
		reply := collectContract(t.Context(), vendor.newModel(server),
			brain.ChatRequest{Messages: contractQuestion, Tools: []brain.ToolSpec{contractWeather}}, nil)
		if reply.err != nil || reply.stop != brain.StopMaxTokens || reply.text() != "从前有座山，" || len(reply.calls) != 0 || reply.usage == nil {
			t.Errorf("截断：%+v", reply)
		}
	})
}

// 流中途断开：报错（不当正常结束），已经说了的照样交出，用量标不完整。
func TestContractDisconnect(t *testing.T) {
	forEachVendor(t, func(t *testing.T, vendor contractVendor) {
		events := vendor.events([]string{"从前", "有座山"}, nil, contractDone)
		server := vendor.newServer(t, llmtest.Script{Events: events, CutAfter: vendor.firstText + 1})
		reply := collectContract(t.Context(), vendor.newModel(server), brain.ChatRequest{Messages: contractQuestion}, nil)
		if reply.err == nil || reply.text() != "从前" || reply.usage == nil || !reply.usage.Incomplete {
			t.Errorf("中途断开：%+v", reply)
		}
	})
}

// 被取消（主人打断）：报取消，用量标不完整。
func TestContractCancel(t *testing.T) {
	forEachVendor(t, func(t *testing.T, vendor contractVendor) {
		events := vendor.events([]string{"从前"}, nil, contractDone)[:vendor.firstText+1]
		server := vendor.newServer(t, llmtest.Script{Events: events, Hang: true})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reply := collectContract(ctx, vendor.newModel(server), brain.ChatRequest{Messages: contractQuestion}, cancel)
		if !errors.Is(reply.err, context.Canceled) || reply.text() != "从前" || reply.usage == nil || !reply.usage.Incomplete {
			t.Errorf("被取消：%+v", reply)
		}
	})
}

// 「工具照给、这轮不许调用」：工具定义和平常一轮完全一样（提示词缓存的前缀不变），并声明不许调用；
// 不认这个选项的服务（Ollama）这一轮不带工具。平常一轮不声明。
func TestContractForbidToolCallsKeepsDefinitions(t *testing.T) {
	forEachVendor(t, func(t *testing.T, vendor contractVendor) {
		script := llmtest.Script{Events: vendor.events([]string{"晴天。"}, nil, contractDone)}
		server := vendor.newServer(t, script, script)
		model := vendor.newModel(server)
		tools := []brain.ToolSpec{contractWeather}
		for _, forbid := range []bool{false, true} {
			if reply := collectContract(t.Context(), model, brain.ChatRequest{Messages: contractQuestion, Tools: tools, ForbidToolCalls: forbid}, nil); reply.err != nil {
				t.Fatal(reply.err)
			}
		}
		normal, forbidden := server.Request(0), server.Request(1)
		if defs := vendor.tools(normal); len(defs) != 1 || vendor.toolName(defs[0]) != "get_weather" || vendor.forbids(normal) {
			t.Errorf("平常一轮带工具、不声明不许调用：%v", normal)
		}
		switch {
		case vendor.omitsForbiddenTools:
			if defs := vendor.tools(forbidden); len(defs) != 0 {
				t.Errorf("不认「不许调用」的服务这一轮不带工具：%v", defs)
			}
		case !reflect.DeepEqual(vendor.tools(forbidden), vendor.tools(normal)) || !vendor.forbids(forbidden):
			t.Errorf("工具定义应和平常一轮完全一样，并声明不许调用：%v", forbidden)
		}
	})
}
