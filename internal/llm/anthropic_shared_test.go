package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// 2026-11-18 集群实测的 bug：聊天、摘要、调查各建一个 Claude 客户端，各自拿同一张集群令牌去换（令牌带 jti，只能换一次），
// 第一个之后全被拒（401）。这几个测试守住「全进程共用一个 ClaudeAPI」。

// 一次只说「好」的回答。
var shortTextEvents = []string{
	`{"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
	`{"type":"message_stop"}`,
}

func writeEvents(w http.ResponseWriter, events []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(event), &head)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, event)
	}
}

// workloadIdentityServer 是假的 Claude API + 换令牌接口：同一张身份令牌只能换一次（和集群令牌带 jti 时一样）。
type workloadIdentityServer struct {
	*httptest.Server
	mu        sync.Mutex
	exchanges int
	used      map[string]bool
}

func newWorkloadIdentityServer(t *testing.T) *workloadIdentityServer {
	t.Helper()
	server := &workloadIdentityServer{used: map[string]bool{}}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			var body struct {
				Assertion string `json:"assertion"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			server.mu.Lock()
			reused := server.used[body.Assertion]
			server.used[body.Assertion] = true
			server.exchanges++
			server.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if reused {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"Authentication failed"}}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"sk-ant-oat01-fake","token_type":"Bearer","expires_in":3600,"scope":"workspace:developer"}`)
		case "/v1/messages":
			if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-fake" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"bad token"}}`)
				return
			}
			writeEvents(w, shortTextEvents)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *workloadIdentityServer) exchangeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exchanges
}

// useWorkloadIdentity 让 SDK 走 Workload Identity Federation（和集群里一样），并清掉会排在它前面的凭证变量。
func useWorkloadIdentity(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_WORKSPACE_ID", "ANTHROPIC_SERVICE_ACCOUNT_ID", "ANTHROPIC_IDENTITY_TOKEN"} {
		if value, ok := os.LookupEnv(name); ok { // 设成空串也会占住它在凭证链里的位置：要真的去掉
			os.Unsetenv(name)
			t.Cleanup(func() { os.Setenv(name, value) })
		}
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("header.payload.signature"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir()) // 不读开发机上真的登录档案
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "fdrl_test")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "00000000-0000-0000-0000-000000000000")
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", tokenFile)
}

func replyText(t *testing.T, model brain.ChatModel) (string, error) {
	t.Helper()
	text, _, _, err := collect(t, model.StreamReply(context.Background(), []brain.ChatMessage{{Role: brain.RoleUser, Text: "?"}}, nil))
	return text, err
}

func TestClaudeAPISharedByModelsExchangesIdentityTokenOnce(t *testing.T) {
	useWorkloadIdentity(t)
	server := newWorkloadIdentityServer(t)
	shared := NewClaudeAPI("", server.URL, server.Client())
	// 不同用途的设置不同（主对话快速失败、调查重试），但共用同一个客户端
	chat := NewAnthropicChatModel(AnthropicOptions{API: shared, Model: "claude-haiku-5-5", MaxTokens: 100, FirstTokenTimeout: 5 * time.Second})
	summary := NewAnthropicChatModel(AnthropicOptions{API: shared, Model: "claude-haiku-5-5", MaxTokens: 300})
	research := NewAnthropicChatModel(AnthropicOptions{API: shared, Model: "claude-haiku-5-5", MaxTokens: 100, MaxRetries: 2})
	for name, model := range map[string]*AnthropicChatModel{"聊天": chat, "摘要": summary, "调查": research} {
		if text, err := replyText(t, model); err != nil || text != "好" {
			t.Errorf("%s：%q %v", name, text, err)
		}
	}
	if got := server.exchangeCount(); got != 1 {
		t.Errorf("共用一个客户端时只应换 1 次令牌，实际 %d 次", got)
	}

	// 阳性对照：各建各的客户端（修之前的做法）⇒ 第二个客户端拿同一张身份令牌去换，被拒
	separate := NewAnthropicChatModel(AnthropicOptions{API: NewClaudeAPI("", server.URL, server.Client()),
		Model: "claude-haiku-5-5", MaxTokens: 100})
	if _, err := replyText(t, separate); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("另建的客户端应因令牌已换过而失败（401），实际 %v", err)
	}
}

// 等不到 Claude 开始回答：在 FirstTokenTimeout 内报错，而且不能被当成「主人打断了」——退路要接手。
func TestAnthropicFirstTokenTimeoutHandsOverToFallback(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // 一直不开始回答
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	model := NewAnthropicChatModel(AnthropicOptions{API: NewClaudeAPI("test-key", slow.URL, slow.Client()),
		Model: "claude-haiku-5-5", MaxTokens: 100, FirstTokenTimeout: 100 * time.Millisecond})

	started := time.Now()
	_, err := replyText(t, model)
	if !errors.Is(err, errNoFirstToken) || errors.Is(err, context.Canceled) {
		t.Fatalf("应报「没等到开始回答」且不包着 context.Canceled：%v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("应在约 100 ms 后放弃，实际等了 %v", elapsed)
	}

	local := &scriptedModel{deltas: []brain.ReplyDelta{{Text: "本地回答"}}}
	fallback := NewFallbackChatModel(model, "cloud", local, "local", time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if text, err := replyText(t, fallback); err != nil || text != "本地回答" {
		t.Errorf("退路应接手：%q %v", text, err)
	}
}

// 缓存断点 ② 打在「上一轮结束处」：这一轮主人的话（附着每轮都变的「现在的情况」）和这一轮的工具往来都在断点之后。
func TestAnthropicMarksPreviousTurnForCache(t *testing.T) {
	model := NewAnthropicChatModel(AnthropicOptions{API: NewClaudeAPI("test-key", "", nil), Model: "claude-haiku-5-5", MaxTokens: 100})
	system := brain.ChatMessage{Role: brain.RoleSystem, Text: "你是 haro。"}
	cases := []struct {
		name     string
		messages []brain.ChatMessage
		marked   string // 应带断点的那一块里出现的文字；空 = 不该有断点
	}{
		{"第一轮：前面没有对话", []brain.ChatMessage{system, {Role: brain.RoleUser, Text: "现在 10 点。你好"}}, ""},
		{"上一轮的回答", []brain.ChatMessage{system,
			{Role: brain.RoleUser, Text: "你好"}, {Role: brain.RoleAssistant, Text: "上一轮的回答"},
			{Role: brain.RoleUser, Text: "现在 10 点。几点了"}}, "上一轮的回答"},
		{"这一轮正在调工具", []brain.ChatMessage{system,
			{Role: brain.RoleUser, Text: "你好"}, {Role: brain.RoleAssistant, Text: "上一轮的回答"},
			{Role: brain.RoleUser, Text: "现在 10 点。天气"},
			{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "w", Name: "get_weather"}}},
			{Role: brain.RoleTool, ToolName: "get_weather", ToolCallID: "w", Text: "晴"}}, "上一轮的回答"},
		{"上一轮有工具往来", []brain.ChatMessage{system,
			{Role: brain.RoleUser, Text: "记一下"},
			{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{{ID: "n", Name: "save_note"}}},
			{Role: brain.RoleTool, ToolName: "save_note", ToolCallID: "n", Text: "已记下"},
			{Role: brain.RoleAssistant, Text: "记下了"},
			{Role: brain.RoleUser, Text: "现在 10 点。还有呢"}}, "记下了"},
	}
	for _, c := range cases {
		params, err := model.request(c.messages, nil)
		if err != nil {
			t.Fatal(err)
		}
		var marked []string
		for _, message := range params.Messages {
			var blocks []string
			raw, _ := json.Marshal(message)
			var decoded struct {
				Content []map[string]any `json:"content"`
			}
			_ = json.Unmarshal(raw, &decoded)
			for _, block := range decoded.Content {
				if block["cache_control"] != nil {
					text, _ := json.Marshal(block)
					blocks = append(blocks, string(text))
				}
			}
			marked = append(marked, blocks...)
		}
		switch {
		case c.marked == "" && len(marked) != 0:
			t.Errorf("%s：不该有断点，实际 %v", c.name, marked)
		case c.marked != "" && (len(marked) != 1 || !strings.Contains(marked[0], c.marked)):
			t.Errorf("%s：断点应只打在「%s」那一块，实际 %v", c.name, c.marked, marked)
		}
	}
}
