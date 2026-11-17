//go:build integration

// 对本机真实 ollama 的集成测试（默认不跑）：
//
//	HARO_TEST_IMAGE=/path/frame.jpg go test -tags integration -run Ollama -v ./internal/llm/
//
// 输出首 token 时间、可见字数、思考字数、token 用量，用来查「看不见的输出」。
package llm

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"haro/internal/brain"
)

func TestOllamaStreamReplyLive(t *testing.T) {
	baseURL := os.Getenv("HARO_OLLAMA_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	modelName := os.Getenv("HARO_MODEL")
	if modelName == "" {
		modelName = "qwen3-vl:4b"
	}
	model := NewOllamaChatModel(baseURL, modelName, 4096, "30m", &http.Client{Timeout: 2 * time.Minute})
	ctx := context.Background()
	if err := model.Warmup(ctx); err != nil {
		t.Fatal(err)
	}
	var image []byte
	if path := os.Getenv("HARO_TEST_IMAGE"); path != "" {
		var err error
		if image, err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}
	history := []brain.ChatMessage{{Role: brain.RoleSystem,
		Text: "你是 Haro，一个住在主人电脑里、能通过摄像头看到主人的小助手。用一两句简短、口语化的中文回答，不要使用表情符号和列表。"}}

	for round := 1; round <= 3; round++ {
		started := time.Now()
		var firstToken time.Duration
		var answer strings.Builder
		var usage *brain.ReplyUsage
		input := brain.ChatMessage{Role: brain.RoleUser, Text: "你能看到我吗？我旁边有什么？", Image: image}
		for delta, err := range model.StreamReply(ctx, append(history, input), nil) {
			if err != nil {
				t.Fatal(err)
			}
			if delta.Text != "" && firstToken == 0 {
				firstToken = time.Since(started)
			}
			answer.WriteString(delta.Text)
			if delta.Usage != nil {
				usage = delta.Usage
			}
		}
		t.Logf("第 %d 轮：首 token %s，总计 %s，带图=%v", round, firstToken.Round(time.Millisecond),
			time.Since(started).Round(time.Millisecond), image != nil)
		t.Logf("  回答（%d 字）：%s", utf8.RuneCountInString(answer.String()), answer.String())
		if usage != nil {
			t.Logf("  prompt_tokens=%d output_tokens=%d thinking_chars=%d load_ms=%d",
				usage.PromptTokens, usage.OutputTokens, usage.ThinkingChars, usage.LoadMillis)
		}
	}
}
