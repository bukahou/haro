package llmtest

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCheckClaudeToolPairs(t *testing.T) {
	use := map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1"}}}
	result := map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1"}}}
	question := map[string]any{"role": "user", "content": "车停哪了"}
	cases := []struct {
		name     string
		messages []any
		ok       bool
	}{
		{"成对", []any{question, use, result}, true},
		{"调用之后直接是别的话", []any{question, use, question}, false},
		{"结果对不上调用", []any{question, result}, false},
		{"最后的调用没有结果", []any{question, use}, false},
	}
	for _, c := range cases {
		if err := CheckClaudeToolPairs(map[string]any{"messages": c.messages}); (err == nil) != c.ok {
			t.Errorf("%s：err = %v", c.name, err)
		}
	}
}

// CutAfter：推送到一半断开，客户端读到的是连接意外中断，而不是正常结束。
func TestServerCutsStreamMidway(t *testing.T) {
	server := NewOpenAIServer(t, Script{Events: []string{`{"n":1}`, `{"n":2}`}, CutAfter: 1})
	response, err := http.Post(server.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err == nil {
		t.Errorf("应读到连接中断，实际正常读完：%q", body)
	}
	if !strings.Contains(string(body), `{"n":1}`) || strings.Contains(string(body), `{"n":2}`) {
		t.Errorf("断开前应只推送了第一个事件：%q", body)
	}
	if server.Count() != 1 {
		t.Errorf("应记下 1 个请求，实际 %d", server.Count())
	}
}
