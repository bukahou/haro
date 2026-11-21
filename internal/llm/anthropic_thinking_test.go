package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/brain"
)

// 各模型「关掉思考」的写法不同：Haiku 5.5 用 disabled，Sonnet 5.5 只认 between_tools（发 disabled 会 400）。
func TestAnthropicThinkingConfig(t *testing.T) {
	cases := map[string]string{
		"":                   `"thinking":{"type":"disabled"}`,
		ThinkingDisabled:     `"thinking":{"type":"disabled"}`,
		ThinkingBetweenTools: `"thinking":{"type":"between_tools"}`,
		ThinkingAdaptive:     `"thinking":{"type":"adaptive"}`,
	}
	for thinking, want := range cases {
		model := NewAnthropicChatModel(AnthropicOptions{API: NewClaudeAPI("test-key", "", nil), Model: "m", MaxTokens: 100,
			Effort: "low", Thinking: thinking})
		params, err := model.request(brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "你好"}}})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(params)
		if !strings.Contains(string(raw), want) {
			t.Errorf("Thinking=%q：请求里应有 %s，实际 %s", thinking, want, raw)
		}
	}
}
