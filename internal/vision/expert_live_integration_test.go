//go:build integration

package vision

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/tools"
)

// 真接口看一张评测样本（花钱：Sonnet 一两次请求，约 ¥1；给了 Tavily 密钥时可能再搜 1–2 次）：
//
//	HARO_ANTHROPIC_API_KEY=… HARO_EVAL_DIR=<样本目录> [HARO_EVAL_SAMPLE=05] [HARO_EVAL_REQUEST=这是什么？] [HARO_TAVILY_API_KEY=…] \
//	  go test -tags integration -run LiveExpert -v ./internal/vision/
func TestLiveExpert(t *testing.T) {
	key, dir := os.Getenv("HARO_ANTHROPIC_API_KEY"), os.Getenv("HARO_EVAL_DIR")
	if key == "" || dir == "" {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY / HARO_EVAL_DIR")
	}
	sample := os.Getenv("HARO_EVAL_SAMPLE")
	if sample == "" {
		sample = "05"
	}
	frame, err := os.ReadFile(filepath.Join(dir, sample+"-1280.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	model := llm.NewAnthropicChatModel(llm.AnthropicOptions{API: llm.NewClaudeAPI(key, "", nil), Model: "claude-sonnet-5-5",
		MaxTokens: 800, Effort: "low", Thinking: llm.ThinkingBetweenTools})
	var search Search
	if tavilyKey := os.Getenv("HARO_TAVILY_API_KEY"); tavilyKey != "" {
		connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		searcher, err := tools.ConnectTavily(connectCtx, "https://mcp.tavily.com/mcp/", tavilyKey, http.DefaultClient, logger)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		defer searcher.Close()
		search = func(ctx context.Context, query string) (string, error) { return searcher.Search(ctx, query, 3) }
	}
	request := os.Getenv("HARO_EVAL_REQUEST")
	if request == "" {
		request = "这是什么？"
	}
	report, err := NewExpert(model, "claude-sonnet-5-5", search, logger).ExamineFrame(ctx, frame, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("专家的判断：%s", report)
}
