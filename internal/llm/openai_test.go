package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"haro/internal/brain"
)

// sseServer 返回一个按 chunks 依次推送 SSE 的假服务，并把收到的请求体交给 inspect。
func sseServer(t *testing.T, chunks []string, inspect func(*http.Request, map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if inspect != nil {
			inspect(r, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func collect(t *testing.T, seq iter.Seq2[brain.ReplyDelta, error]) (string, []brain.ToolCall, *brain.ReplyUsage, error) {
	t.Helper()
	var text strings.Builder
	var calls []brain.ToolCall
	var usage *brain.ReplyUsage
	for delta, err := range seq {
		if err != nil {
			return text.String(), calls, usage, err
		}
		text.WriteString(delta.Text)
		calls = append(calls, delta.ToolCalls...)
		if delta.Usage != nil {
			usage = delta.Usage
		}
	}
	return text.String(), calls, usage, nil
}

func TestOpenAIStreamsTextAndUsage(t *testing.T) {
	server := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"你好，"}}]}`,
		`{"choices":[{"delta":{"content":"我在。"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":8}}`,
	}, func(r *http.Request, body map[string]any) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("请求 %s，鉴权头 %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if body["stream"] != true || body["model"] != "qwen.test" {
			t.Errorf("请求体 %v", body)
		}
	})
	defer server.Close()
	model := NewOpenAIChatModel(server.URL+"/v1/", "qwen.test", "test-key", 300, server.Client())
	text, calls, usage, err := collect(t, model.StreamReply(context.Background(),
		[]brain.ChatMessage{{Role: brain.RoleUser, Text: "你好"}}, nil))
	if err != nil || text != "你好，我在。" || len(calls) != 0 || usage == nil || usage.PromptTokens != 1200 {
		t.Errorf("text=%q calls=%v usage=%+v err=%v", text, calls, usage, err)
	}
}

func TestOpenAIAccumulatesStreamedToolCall(t *testing.T) {
	server := sseServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"abc","type":"function","function":{"name":"save_","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"note","arguments":"{\"content\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"车停在B2\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}, nil)
	defer server.Close()
	model := NewOpenAIChatModel(server.URL, "qwen.test", "k", 300, server.Client())
	_, calls, _, err := collect(t, model.StreamReply(context.Background(), []brain.ChatMessage{{Role: brain.RoleUser, Text: "记一下"}},
		[]brain.ToolSpec{{Name: "save_note", Parameters: json.RawMessage(`{"type":"object"}`)}}))
	if err != nil || len(calls) != 1 || calls[0].Name != "save_note" || calls[0].ID != "abc" || calls[0].Arguments["content"] != "车停在B2" {
		t.Errorf("calls=%+v err=%v", calls, err)
	}
}

func TestOpenAIRequestShape(t *testing.T) {
	var sent map[string]any
	server := sseServer(t, []string{`{"choices":[{"delta":{"content":"好"}}]}`}, func(_ *http.Request, body map[string]any) { sent = body })
	defer server.Close()
	messages := []brain.ChatMessage{
		{Role: brain.RoleSystem, Text: "系统"},
		{Role: brain.RoleUser, Text: "我车停哪了"},
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "call_7", Name: "search_notes", Arguments: map[string]any{"query": "车"}}}},
		{Role: brain.RoleTool, ToolName: "search_notes", ToolCallID: "call_7", Text: "B2"},
		{Role: brain.RoleAssistant, Text: "在B2。"},
		{Role: brain.RoleUser, Text: "你看到什么？", Image: []byte("jpeg")},
	}
	model := NewOpenAIChatModel(server.URL, "moonshotai.kimi-k2.5", "k", 300, server.Client())
	if _, _, _, err := collect(t, model.StreamReply(context.Background(), messages, nil)); err != nil {
		t.Fatal(err)
	}
	wire := sent["messages"].([]any)
	assistant := wire[2].(map[string]any)
	tool := wire[3].(map[string]any)
	callID := assistant["tool_calls"].([]any)[0].(map[string]any)["id"]
	// Kimi：ID 要重写成它自己的格式，调用与结果同步改
	if callID != "functions.search_notes:0" || tool["tool_call_id"] != callID || assistant["content"] != nil {
		t.Errorf("工具往来 assistant=%v tool=%v", assistant, tool)
	}
	content := wire[5].(map[string]any)["content"].([]any)
	image := content[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(image, "data:image/jpeg;base64,") {
		t.Errorf("图片应以 data URI 发送：%v", content)
	}
}

func TestToolCallIDStyles(t *testing.T) {
	cases := map[string]toolCallIDStyle{"qwen.qwen3-vl-235b-a22b": idStyleOpaque, "moonshotai.kimi-k2.5": idStyleKimi,
		"mistral.ministral-3-14b": idStyleMistral}
	for model, want := range cases {
		if got := toolCallIDStyleFor(model); got != want {
			t.Errorf("%s → %v，应为 %v", model, got, want)
		}
	}
	messages := toOpenAIMessages([]brain.ChatMessage{
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "x", Name: "save_note"}}},
		{Role: brain.RoleTool, ToolName: "save_note", ToolCallID: "x"},
	}, idStyleMistral)
	if id := messages[0].ToolCalls[0].ID; len(id) != 9 || messages[1].ToolCallID != id {
		t.Errorf("Mistral 的 ID 应为 9 位且调用与结果一致：%q / %q", id, messages[1].ToolCallID)
	}
}

func TestOpenAIHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"AccessDeniedException"}`, http.StatusForbidden)
	}))
	defer server.Close()
	model := NewOpenAIChatModel(server.URL, "m", "bad", 300, server.Client())
	if _, _, _, err := collect(t, model.StreamReply(context.Background(), []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}, nil)); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("应返回 403 错误，得到 %v", err)
	}
}

// ——— 主备切换 ———

type scriptedModel struct {
	deltas []brain.ReplyDelta
	err    error
	calls  int
}

func (s *scriptedModel) StreamReply(context.Context, []brain.ChatMessage, []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	s.calls++
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, delta := range s.deltas {
			if !yield(delta, nil) {
				return
			}
		}
		if s.err != nil {
			yield(brain.ReplyDelta{}, s.err)
		}
	}
}
func (s *scriptedModel) Warmup(context.Context) error { return nil }

func TestFallbackSwitchesOnlyBeforeAnyOutput(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	input := []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}

	// 主模型一开始就失败 → 这一轮用备用模型
	primary := &scriptedModel{err: errors.New("断网")}
	secondary := &scriptedModel{deltas: []brain.ReplyDelta{{Text: "本地回答"}}}
	text, _, _, err := collect(t, NewFallbackChatModel(primary, "cloud", secondary, "local", 0, logger).StreamReply(context.Background(), input, nil))
	if err != nil || text != "本地回答" {
		t.Errorf("应退回本地：text=%q err=%v", text, err)
	}

	// 主模型说到一半才失败 → 不切换（避免说两遍），报错
	primary = &scriptedModel{deltas: []brain.ReplyDelta{{Text: "说了一半"}}, err: errors.New("连接断了")}
	secondary = &scriptedModel{deltas: []brain.ReplyDelta{{Text: "不该出现"}}}
	text, _, _, err = collect(t, NewFallbackChatModel(primary, "cloud", secondary, "local", 0, logger).StreamReply(context.Background(), input, nil))
	if err == nil || text != "说了一半" || secondary.calls != 0 {
		t.Errorf("中途失败不应切换：text=%q err=%v secondary.calls=%d", text, err, secondary.calls)
	}

	// 主模型正常 → 不碰备用模型
	primary = &scriptedModel{deltas: []brain.ReplyDelta{{Text: "云端回答"}}}
	secondary = &scriptedModel{}
	text, _, _, _ = collect(t, NewFallbackChatModel(primary, "cloud", secondary, "local", 0, logger).StreamReply(context.Background(), input, nil))
	if text != "云端回答" || secondary.calls != 0 {
		t.Errorf("text=%q secondary.calls=%d", text, secondary.calls)
	}
}

func TestFallbackCircuitBreaker(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	input := []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}
	primary := &scriptedModel{err: errors.New("断网")}
	secondary := &scriptedModel{deltas: []brain.ReplyDelta{{Text: "本地"}}}
	model := NewFallbackChatModel(primary, "cloud", secondary, "local", 100*time.Millisecond, logger)

	collect(t, model.StreamReply(context.Background(), input, nil)) // 主模型失败 → 熔断
	collect(t, model.StreamReply(context.Background(), input, nil)) // 熔断中：不应再试主模型
	if primary.calls != 1 || secondary.calls != 2 {
		t.Fatalf("熔断中不应再试主模型：primary=%d secondary=%d", primary.calls, secondary.calls)
	}
	time.Sleep(150 * time.Millisecond) // 过了冷却期：再试一次主模型
	primary.err, primary.deltas = nil, []brain.ReplyDelta{{Text: "云端"}}
	if text, _, _, _ := collect(t, model.StreamReply(context.Background(), input, nil)); text != "云端" || primary.calls != 2 {
		t.Errorf("冷却期后应切回主模型：text=%q primary=%d", text, primary.calls)
	}
}

// warmupModel 记下有没有被预热。
type warmupModel struct {
	scriptedModel
	warmed bool
	err    error
}

func (w *warmupModel) Warmup(context.Context) error { w.warmed = true; return w.err }

// v4 D9：本地当主模型时也要预热它（以前只预热备用的本地模型）；主模型预热失败不影响备用。
func TestFallbackWarmsBothModels(t *testing.T) {
	primary, secondary := &warmupModel{err: errors.New("本地模型那台机器离线了")}, &warmupModel{}
	model := NewFallbackChatModel(primary, "local", secondary, "cloud", time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := model.Warmup(context.Background()); err == nil || !primary.warmed || !secondary.warmed {
		t.Errorf("主备都应预热、主模型的错误要报出来：err=%v primary=%v secondary=%v", err, primary.warmed, secondary.warmed)
	}
}
