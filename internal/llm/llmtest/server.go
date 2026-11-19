// Package llmtest 是测试模型适配层（internal/llm）用的假服务端：按脚本推送流式事件、记下收到的请求，
// 可以在推送到一半时断开连接；每个请求都检查工具调用和工具结果是否成对。只给测试用。
package llmtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// Server 是一个按脚本回答的假模型服务。
type Server struct {
	*httptest.Server
	t testing.TB

	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
}

// Requests 是收到的全部请求体（按 JSON 解开）。
func (s *Server) Requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Request 是第 index 个请求体；不够时终止测试。
func (s *Server) Request(index int) map[string]any {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.requests) {
		s.t.Fatalf("假服务只收到 %d 个请求", len(s.requests))
	}
	return s.requests[index]
}

// Header 是第 index 个请求的请求头；不够时终止测试。
func (s *Server) Header(index int) http.Header {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.headers) {
		s.t.Fatalf("假服务只收到 %d 个请求", len(s.headers))
	}
	return s.headers[index]
}

// Count 是收到的请求数。
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *Server) record(r *http.Request) map[string]any {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.requests = append(s.requests, body)
	s.headers = append(s.headers, r.Header.Clone())
	s.mu.Unlock()
	return body
}

// Script 是一次回答要推送的内容。
type Script struct {
	Status   int      // 不是 200 时只回这个状态码和一段错误 JSON
	Events   []string // 依次推送的事件（JSON）
	CutAfter int      // 大于 0 时，推送这么多个事件后直接断开连接（模拟流中途断开）
}

// NewClaudeServer 是假的 Claude Messages API（POST /v1/messages）：按 Script 推送 SSE，「event:」行取自每个事件的 type。
// 第 N 个请求用第 N 个 Script；请求比 Script 多时重复用最后一个。
func NewClaudeServer(t testing.TB, scripts ...Script) *Server {
	t.Helper()
	server := &Server{t: t}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := server.record(r)
		if r.URL.Path != "/v1/messages" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if err := CheckClaudeToolPairs(body); err != nil {
			t.Errorf("发给 Claude 的消息不成对：%v", err)
		}
		script := pick(scripts, server.Count()-1)
		if script.Status != 0 && script.Status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(script.Status)
			fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, event := range script.Events {
			if script.CutAfter > 0 && i == script.CutAfter {
				cut(w)
			}
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(event), &head); err != nil {
				t.Errorf("测试事件不是合法 JSON: %v\n%s", err, event)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, event)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// NewOpenAIServer 是假的 OpenAI 兼容接口：按 Script 推送「data:」行，最后推「data: [DONE]」。
func NewOpenAIServer(t testing.TB, scripts ...Script) *Server {
	t.Helper()
	server := &Server{t: t}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.record(r)
		script := pick(scripts, server.Count()-1)
		if script.Status != 0 && script.Status != http.StatusOK {
			http.Error(w, `{"message":"error"}`, script.Status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, event := range script.Events {
			if script.CutAfter > 0 && i == script.CutAfter {
				cut(w)
			}
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

func pick(scripts []Script, index int) Script {
	if len(scripts) == 0 {
		return Script{}
	}
	return scripts[min(index, len(scripts)-1)]
}

// cut 把已经写出的部分推给客户端，然后中断这次响应（客户端看到的是连接意外断开）。
func cut(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	panic(http.ErrAbortHandler)
}

// CheckClaudeToolPairs 检查 Claude 请求里的工具往来：每个 tool_use 之后的下一条（用户）消息里都有对应的 tool_result；
// 每个 tool_result 都对得上紧挨着的上一条助手消息里的 tool_use。
func CheckClaudeToolPairs(body map[string]any) error {
	messages, _ := body["messages"].([]any)
	var pending []string
	for i, raw := range messages {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		blocks, _ := message["content"].([]any)
		var uses, results []string
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			switch block["type"] {
			case "tool_use":
				id, _ := block["id"].(string)
				uses = append(uses, id)
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				results = append(results, id)
			}
		}
		for _, id := range results {
			if !slices.Contains(pending, id) {
				return fmt.Errorf("第 %d 条消息的 tool_result %q 对不上上一条的 tool_use", i, id)
			}
		}
		for _, id := range pending {
			if !slices.Contains(results, id) {
				return fmt.Errorf("第 %d 条消息缺 tool_use %q 的结果", i, id)
			}
		}
		pending = nil
		if role == "assistant" {
			pending = uses
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("最后的 tool_use %v 没有结果", pending)
	}
	return nil
}
