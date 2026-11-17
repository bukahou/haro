package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
)

// fakeTavily 是假的 Tavily 远程 MCP（Streamable HTTP）：只认 Bearer 密钥，记下收到的参数。
func fakeTavily(t *testing.T, wantKey string, lastQuery *atomic.Value) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-tavily", Version: "0.0.1"}, nil)
	type searchInput struct {
		Query             string `json:"query"`
		MaxResults        int    `json:"max_results"`
		SearchDepth       string `json:"search_depth"`
		IncludeRawContent bool   `json:"include_raw_content"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "tavily_search"}, func(_ context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
		lastQuery.Store(fmt.Sprintf("%s|%d|%s|%v", in.Query, in.MaxResults, in.SearchDepth, in.IncludeRawContent))
		body, _ := json.Marshal(map[string]any{"request_id": "x", "images": []any{}, "results": []map[string]any{
			{"title": "日经平均", "url": "https://example.com/n", "content": strings.Repeat("涨", 5000), "score": 0.9}}})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})
	type extractInput struct {
		URLs  []string `json:"urls"`
		Query string   `json:"query"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "tavily_extract"}, func(_ context.Context, _ *mcp.CallToolRequest, in extractInput) (*mcp.CallToolResult, any, error) {
		lastQuery.Store("extract:" + in.Query)
		var results []map[string]any
		for i, url := range in.URLs {
			results = append(results, map[string]any{"url": url, "raw_content": strings.Repeat("文", 10000*(i+1))})
		}
		body, _ := json.Marshal(map[string]any{"results": results, "failed_results": []map[string]any{{"url": "https://bad.example", "error": "404"}}})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
}

// countingMeter 记下额度检查与记账；allowErr 非空时拒绝。
type countingMeter struct {
	allowErr error
	recorded int
}

func (m *countingMeter) AllowSearch(context.Context, int) error { return m.allowErr }
func (m *countingMeter) RecordSearch(_ context.Context, credits int) error {
	m.recorded += credits
	return nil
}

func TestTavilyQuickSearch(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var lastQuery atomic.Value
	server := fakeTavily(t, "tvly-test", &lastQuery)
	defer server.Close()

	if _, err := ConnectTavily(ctx, server.URL, "tvly-wrong", nil, logger); err == nil {
		t.Fatal("密钥不对应连不上")
	}
	searcher, err := ConnectTavily(ctx, server.URL, "tvly-test", nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer searcher.Close()

	meter := &countingMeter{}
	box, err := NewToolbox(ctx, nil, logger, WebSearchTools(searcher, meter, logger)...)
	if err != nil {
		t.Fatal(err)
	}
	result, err := box.Call(ctx, brain.ToolCall{Name: "web_search", Arguments: map[string]any{"query": "日经平均"}})
	if err != nil || !strings.Contains(result, `"title":"日经平均"`) || strings.Contains(result, "request_id") ||
		!strings.HasSuffix(result, "…（后面省略）") {
		t.Fatalf("web_search：%.80s… %v", result, err)
	}
	if got := lastQuery.Load(); got != "日经平均|3|basic|false" {
		t.Errorf("快查应固定参数（3 条、basic、不带全文）：%v", got)
	}
	if meter.recorded != 1 {
		t.Errorf("一次基础搜索记 1 积分：%d", meter.recorded)
	}

	meter.allowErr = errors.New("今天的搜索用完了")
	if _, err := box.Call(ctx, brain.ToolCall{Name: "web_search", Arguments: map[string]any{"query": "x"}}); err == nil || meter.recorded != 1 {
		t.Errorf("额度不够时不搜、不记账：%v recorded=%d", err, meter.recorded)
	}

	pages, err := searcher.ReadPages(ctx, []string{"https://example.com/a", "https://example.com/b"}, 2000, "要找的内容")
	var read struct {
		Pages []struct{ URL, Content, Error string } `json:"pages"`
	}
	if err != nil || json.Unmarshal([]byte(pages), &read) != nil || len(read.Pages) != 3 {
		t.Fatalf("抓正文：%.200s %v", pages, err)
	}
	for _, page := range read.Pages[:2] {
		if n := len([]rune(page.Content)); n != 2000+len([]rune("…（后面省略）")) {
			t.Errorf("每页单独截到 2000 字：%s %d", page.URL, n)
		}
	}
	if read.Pages[2].Error != "404" {
		t.Errorf("失败的网页要告诉模型：%+v", read.Pages[2])
	}
	if got := lastQuery.Load(); got != "extract:要找的内容" {
		t.Errorf("读网页要把要找的内容传给 Tavily（按相关度挑段落）：%v", got)
	}
	if searcher.ReadCredits(1) != 1 || searcher.ReadCredits(5) != 1 || searcher.ReadCredits(6) != 2 {
		t.Error("抓取每 5 页 1 积分")
	}
}
