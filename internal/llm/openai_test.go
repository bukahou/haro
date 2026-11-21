package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/llm/llmtest"
)

func testOpenAIModel(server *llmtest.Server, options OpenAIOptions) *OpenAIChatModel {
	options.BaseURL, options.HTTPClient = server.URL+"/v1/", server.Client()
	if options.Model == "" {
		options.Model = "qwen.test"
	}
	if options.MaxTokens == 0 {
		options.MaxTokens = 300
	}
	return NewOpenAIChatModel(options)
}

func TestOpenAIStreamsTextAndUsage(t *testing.T) {
	server := llmtest.NewOpenAIServer(t, llmtest.Script{Events: []string{
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"你好，"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"我在。"},"finish_reason":"stop"}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":8,"total_tokens":1208,"prompt_tokens_details":{"cached_tokens":1000}}}`,
	}})
	model := testOpenAIModel(server, OpenAIOptions{APIKey: "test-key"})
	text, calls, usage, err := collect(t, model.StreamReply(context.Background(),
		brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "你好"}}}))
	if err != nil || text != "你好，我在。" || len(calls) != 0 || usage == nil || usage.PromptTokens != 1200 ||
		usage.OutputTokens != 8 || usage.CacheReadTokens != 1000 || usage.TokensUnknown {
		t.Errorf("text=%q calls=%v usage=%+v err=%v", text, calls, usage, err)
	}
	if got := server.Header(0).Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("鉴权头 %q", got)
	}
	body := server.Request(0)
	options, _ := body["stream_options"].(map[string]any)
	if body["stream"] != true || body["model"] != "qwen.test" || body["max_tokens"] != float64(300) || options["include_usage"] != true {
		t.Errorf("请求体 %v", body)
	}
}

func TestOpenAIAccumulatesStreamedToolCall(t *testing.T) {
	server := llmtest.NewOpenAIServer(t, llmtest.Script{Events: []string{
		`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"abc","type":"function","function":{"name":"save_note","arguments":""}}]}}]}`,
		`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"content\":"}}]}}]}`,
		`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"车停在B2\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}})
	model := testOpenAIModel(server, OpenAIOptions{APIKey: "k"})
	_, calls, _, err := collect(t, model.StreamReply(context.Background(), brain.ChatRequest{
		Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "记一下"}},
		Tools:    []brain.ToolSpec{{Name: "save_note", Parameters: json.RawMessage(`{"type":"object"}`)}}}))
	if err != nil || len(calls) != 1 || calls[0].Name != "save_note" || calls[0].ID != "abc" || calls[0].Arguments["content"] != "车停在B2" {
		t.Errorf("calls=%+v err=%v", calls, err)
	}
}

func TestOpenAIRequestShape(t *testing.T) {
	server := llmtest.NewOpenAIServer(t, llmtest.Script{Events: []string{`{"id":"c3","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}]}`}})
	messages := []brain.ChatMessage{
		{Role: brain.RoleSystem, Text: "系统"},
		{Role: brain.RoleUser, Text: "我车停哪了"},
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "call_7", Name: "search_notes", Arguments: map[string]any{"query": "车"}}}},
		{Role: brain.RoleTool, ToolName: "search_notes", ToolCallID: "call_7", Text: "B2"},
		{Role: brain.RoleAssistant, Text: "在B2。"},
		{Role: brain.RoleUser, Text: "你看到什么？", Image: []byte("jpeg")},
	}
	model := testOpenAIModel(server, OpenAIOptions{Model: "moonshotai.kimi-k2.5", APIKey: "k", ToolCallIDs: ToolCallIDsKimi})
	if _, _, _, err := collect(t, model.StreamReply(context.Background(), brain.ChatRequest{Messages: messages})); err != nil {
		t.Fatal(err)
	}
	wire := server.Request(0)["messages"].([]any)
	assistant := wire[2].(map[string]any)
	tool := wire[3].(map[string]any)
	callID := assistant["tool_calls"].([]any)[0].(map[string]any)["id"]
	// 配置成 Kimi 的格式：编号重写成它要的样子，调用与结果同步改；只有调用的那条不带文字
	if callID != "functions.search_notes:0" || tool["tool_call_id"] != callID || assistant["content"] != nil {
		t.Errorf("工具往来 assistant=%v tool=%v", assistant, tool)
	}
	content := wire[5].(map[string]any)["content"].([]any)
	image := content[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(image, "data:image/jpeg;base64,") {
		t.Errorf("图片应以 data URI 发送：%v", content)
	}
}

// 编号格式由配置显式指定（不按模型名猜）；Mistral 要恰好 9 位，调用与结果一致。
func TestToolCallIDStyles(t *testing.T) {
	for value, want := range map[string]ToolCallIDStyle{"": ToolCallIDsOpaque, "opaque": ToolCallIDsOpaque, "kimi": ToolCallIDsKimi, "mistral": ToolCallIDsMistral} {
		if got, err := ParseToolCallIDStyle(value); err != nil || got != want {
			t.Errorf("%q → %v %v，应为 %v", value, got, err, want)
		}
	}
	if _, err := ParseToolCallIDStyle("qwen"); err == nil {
		t.Error("不认识的格式应报错")
	}
	messages := toOpenAIMessages([]brain.ChatMessage{
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "x", Name: "save_note"}}},
		{Role: brain.RoleTool, ToolName: "save_note", ToolCallID: "x"},
	}, ToolCallIDsMistral)
	id := messages[0].OfAssistant.ToolCalls[0].OfFunction.ID
	if len(id) != 9 || messages[1].OfTool.ToolCallID != id {
		t.Errorf("Mistral 的编号应为 9 位且调用与结果一致：%q / %q", id, messages[1].OfTool.ToolCallID)
	}
}

func TestOpenAIHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"AccessDeniedException"}`, http.StatusForbidden)
	}))
	defer server.Close()
	model := NewOpenAIChatModel(OpenAIOptions{BaseURL: server.URL, Model: "m", APIKey: "bad", HTTPClient: server.Client()})
	_, _, usage, err := collect(t, model.StreamReply(context.Background(), brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}}))
	if err == nil || !strings.Contains(err.Error(), "403") || usage == nil || !usage.TokensUnknown {
		t.Errorf("应返回 403 错误、用量记不知道，得到 %v %+v", err, usage)
	}
}

// 不配 key 的服务（本地 Ollama）：不把 SDK 从环境变量读到的别处的 key 带过去；走 Ollama 的 /v1 端点。
func TestOllamaDoesNotForwardAmbientAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-somewhere-else")
	var path atomic.Value
	server := llmtest.NewOpenAIServer(t, llmtest.Script{Events: []string{`{"id":"c4","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}]}`}})
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		server.Config.Handler.ServeHTTP(w, r)
	}))
	defer wrapped.Close()
	model := NewOllamaChatModel(wrapped.URL, "qwen3-vl:8b-instruct", 300, OpenAIOptions{HTTPClient: wrapped.Client()})
	if _, _, _, err := collect(t, model.StreamReply(context.Background(), brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}})); err != nil {
		t.Fatal(err)
	}
	if got := server.Header(0).Get("Authorization"); got != "" {
		t.Errorf("不该带别处的 key：%q", got)
	}
	if got := path.Load(); got != "/v1/chat/completions" {
		t.Errorf("应走 Ollama 的 OpenAI 兼容端点：%v", got)
	}
}

// 思考内容（放在单独字段里的）只计字数，不当回答念。
func TestOpenAICountsReasoningSeparately(t *testing.T) {
	server := llmtest.NewOpenAIServer(t, llmtest.Script{Events: []string{
		`{"id":"c5","choices":[{"index":0,"delta":{"content":"","reasoning":"想一想"}}]}`,
		`{"id":"c5","choices":[{"index":0,"delta":{"content":"","reasoning_content":"再想"}}]}`,
		`{"id":"c5","choices":[{"index":0,"delta":{"content":"是2。"},"finish_reason":"stop"}]}`,
		`{"id":"c5","choices":[],"usage":{"prompt_tokens":19,"completion_tokens":30,"total_tokens":49}}`,
	}})
	model := testOpenAIModel(server, OpenAIOptions{})
	text, _, usage, err := collect(t, model.StreamReply(context.Background(), brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "1+1"}}}))
	if err != nil || text != "是2。" || usage == nil || usage.ThinkingChars != 5 {
		t.Errorf("思考只计字数：text=%q usage=%+v err=%v", text, usage, err)
	}
}

// 预热：按需加载模型的服务（Ollama）发一个只要 1 个 token 的请求；云端什么都不发。
func TestOpenAIWarmupOnlyForOnDemandModels(t *testing.T) {
	var requests atomic.Int32
	var maxTokens atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		maxTokens.Store(body["max_tokens"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"w","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"你"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	defer server.Close()
	cloud := NewOpenAIChatModel(OpenAIOptions{BaseURL: server.URL, Model: "m", APIKey: "k", HTTPClient: server.Client()})
	if err := brain.WarmUp(context.Background(), cloud); err != nil || requests.Load() != 0 {
		t.Errorf("云端不用预热：%v，请求了 %d 次", err, requests.Load())
	}
	local := NewOllamaChatModel(server.URL, "qwen3-vl:8b-instruct", 300, OpenAIOptions{HTTPClient: server.Client()})
	if err := brain.WarmUp(context.Background(), local); err != nil || requests.Load() != 1 || maxTokens.Load() != float64(1) {
		t.Errorf("Ollama 预热应发一个只要 1 个 token 的请求：%v，请求了 %d 次，max_tokens=%v", err, requests.Load(), maxTokens.Load())
	}
}
