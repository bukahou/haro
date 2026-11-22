//go:build integration

package vision

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/tools"
)

// 真接口看一张评测样本（花钱：Sonnet 一两次请求，约 ¥1；给了 Tavily 密钥时可能再搜 1–2 次）：
//
//	HARO_ANTHROPIC_API_KEY=… HARO_EVAL_DIR=<样本目录> [HARO_EVAL_SAMPLE=05] [HARO_EVAL_REQUEST=这是什么？] [HARO_EVAL_STYLE=<性格说明>] [HARO_TAVILY_API_KEY=…] \
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
	api, err := llm.NewClaudeAPI(key, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	model := llm.NewAnthropicChatModel(llm.AnthropicOptions{API: api, Model: "claude-sonnet-5-5",
		MaxTokens: 800, Effort: "low", Thinking: llm.ThinkingBetweenTools})
	var search Search
	if tavilyKey := os.Getenv("HARO_TAVILY_API_KEY"); tavilyKey != "" {
		searcher := tools.NewTavilySearcher("https://mcp.tavily.com/mcp/", tavilyKey, http.DefaultClient, logger)
		defer searcher.Close()
		search = func(ctx context.Context, query string) (string, error) {
			return searcher.Search(ctx, query, 3, loggedSearches{t})
		}
	}
	request := os.Getenv("HARO_EVAL_REQUEST")
	if request == "" {
		request = "这是什么？"
	}
	expert, err := NewExpert(model, "claude-sonnet-5-5", builtinRules(t), search, logger)
	if err != nil {
		t.Fatal(err)
	}
	report, err := expert.ExamineFrame(ctx, brain.FrameQuestion{Frame: frame, Request: request, Style: os.Getenv("HARO_EVAL_STYLE")})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("说给主人听的（%d 字）：%s\n细节：%s", utf8.RuneCountInString(report.Spoken), report.Spoken, report.Details)
}

// loggedSearches 把每次发出去的搜索请求写进测试日志（这里不接账本，花的积分照样看得见）。
type loggedSearches struct{ t *testing.T }

func (l loggedSearches) RecordSearch(_ context.Context, credits int, incomplete bool) error {
	l.t.Logf("搜索用了 %d 积分（没拿到结果：%v）", credits, incomplete)
	return nil
}
