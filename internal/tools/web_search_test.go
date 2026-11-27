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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
)

// fakeTavily 是假的 Tavily 远程 MCP（Streamable HTTP）：只认 Bearer 密钥，记下收到的参数。
// 返回的 restart 像 Tavily 重启那样忘掉所有会话（之前的会话再来请求就是「会话不存在」）。
func fakeTavily(t *testing.T, wantKey string, lastQuery *atomic.Value) (*httptest.Server, func()) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-tavily", Version: "0.0.1"}, nil)
	type searchInput struct {
		Query             string `json:"query"`
		MaxResults        int    `json:"max_results"`
		SearchDepth       string `json:"search_depth"`
		IncludeRawContent bool   `json:"include_raw_content"`
		Country           string `json:"country,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "tavily_search"}, func(_ context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
		recorded := fmt.Sprintf("%s|%d|%s|%v", in.Query, in.MaxResults, in.SearchDepth, in.IncludeRawContent)
		if in.Country != "" {
			recorded += "|" + in.Country
		}
		lastQuery.Store(recorded)
		switch in.Query {
		case badQuery:
			return nil, nil, errors.New("query is invalid")
		case oddQuery:
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"answer":"改版后的格式"}`}}}, nil, nil
		}
		body, _ := json.Marshal(map[string]any{"request_id": "x", "images": []any{}, "results": []map[string]any{
			{"title": "日经平均", "url": "https://example.com/n", "content": strings.Repeat("涨", 5000), "score": 0.9}}})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})
	type extractInput struct {
		URLs         []string `json:"urls"`
		Query        string   `json:"query"`
		ExtractDepth string   `json:"extract_depth"`
		Format       string   `json:"format,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "tavily_extract"}, func(_ context.Context, _ *mcp.CallToolRequest, in extractInput) (*mcp.CallToolResult, any, error) {
		recorded := "extract:" + in.Query
		if in.ExtractDepth != "basic" {
			recorded += "|" + in.ExtractDepth
		}
		if in.Format != "text" { // 要的不是纯文本时记下来（断言里出现就说明没要纯文本）
			recorded += "|format=" + in.Format
		}
		lastQuery.Store(recorded)
		if slices.Contains(in.URLs, oddURL) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "不是 JSON"}}}, nil, nil
		}
		var results []map[string]any
		for i, url := range in.URLs {
			results = append(results, map[string]any{"url": url, "raw_content": strings.Repeat("文", 10000*(i+1))})
		}
		body, _ := json.Marshal(map[string]any{"results": results, "failed_results": []map[string]any{{"url": "https://bad.example", "error": "404"}}})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})
	var handler atomic.Pointer[mcp.StreamableHTTPHandler]
	restart := func() {
		handler.Store(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	}
	restart()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.Load().ServeHTTP(w, r)
	})), restart
}

// badQuery 是假 Tavily 报「这次调用有错」的搜索词（服务正常回应、说参数不对）；oddQuery、oddURL 让它回一个认不出的格式
// （像服务改版了那样）。
const (
	badQuery = "坏的搜索词"
	oddQuery = "格式变了"
	oddURL   = "https://odd.example"
)

// searchCharge 是记下的一笔搜索。
type searchCharge struct {
	credits    int
	incomplete bool
}

// countingMeter 记下额度检查与每一笔记账；allowErr 非空时拒绝。
type countingMeter struct {
	mu       sync.Mutex
	allowErr error
	charges  []searchCharge
}

func (m *countingMeter) AllowSearch(context.Context, int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allowErr
}

func (m *countingMeter) RecordSearch(_ context.Context, credits int, incomplete bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.charges = append(m.charges, searchCharge{credits: credits, incomplete: incomplete})
	return nil
}

func (m *countingMeter) refuse(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowErr = err
}

// recorded 是记下的积分合计。
func (m *countingMeter) recorded() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, charge := range m.charges {
		total += charge.credits
	}
	return total
}

func (m *countingMeter) all() []searchCharge {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.charges)
}

func TestTavilyQuickSearch(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var lastQuery atomic.Value
	server, _ := fakeTavily(t, "tvly-test", &lastQuery)
	defer server.Close()

	refused := &countingMeter{}
	if _, err := NewTavilySearcher(server.URL, "tvly-wrong", nil, logger).Search(ctx, "x", 3, refused); err == nil {
		t.Fatal("密钥不对应连不上")
	}
	if got := refused.all(); len(got) != 0 {
		t.Errorf("连都没连上：请求没发出去，不记账：%+v", got)
	}
	searcher := NewTavilySearcher(server.URL, "tvly-test", nil, logger)
	defer searcher.Close()

	meter := &countingMeter{}
	box, err := NewToolbox(ctx, logger, WebSearchTools(searcher, meter, logger))
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
	if got := meter.all(); !slices.Equal(got, []searchCharge{{credits: 1}}) {
		t.Errorf("一次基础搜索记 1 积分：%+v", got)
	}

	meter.refuse(errors.New("今天的搜索用完了"))
	if _, err := box.Call(ctx, brain.ToolCall{Name: "web_search", Arguments: map[string]any{"query": "x"}}); err == nil || meter.recorded() != 1 {
		t.Errorf("额度不够时不搜、不记账：%v recorded=%d", err, meter.recorded())
	}

	pages, err := searcher.ReadPages(ctx, []string{"https://example.com/a", "https://example.com/b"}, 2000, "要找的内容", meter)
	if err != nil || len(pages) != 3 {
		t.Fatalf("抓正文：%+v %v", pages, err)
	}
	for _, page := range pages[:2] {
		if n := len([]rune(page.Content)); n != 2000+len([]rune("…（后面省略）")) || page.Error != "" {
			t.Errorf("每页单独截到 2000 字：%s %d", page.URL, n)
		}
	}
	if pages[2].URL != "https://bad.example" || pages[2].Error != "404" || pages[2].Content != "" {
		t.Errorf("没读到的网页要带上原因：%+v", pages[2])
	}
	if got := lastQuery.Load(); got != "extract:要找的内容" {
		t.Errorf("读网页要把要找的内容传给 Tavily（按相关度挑段落）：%v", got)
	}
	if searcher.ReadCredits(1) != 1 || searcher.ReadCredits(5) != 1 || searcher.ReadCredits(6) != 2 {
		t.Error("抓取每 5 页 1 积分")
	}
	if got := meter.all(); len(got) != 2 || got[1] != (searchCharge{credits: 1}) {
		t.Errorf("读两页记 1 积分：%+v", got)
	}

	results, err := searcher.Search(ctx, "日经平均", 5, meter)
	if err != nil || len(results) != 1 || results[0].Title != "日经平均" || results[0].URL != "https://example.com/n" ||
		len([]rune(results[0].Snippet)) != 5000 {
		t.Errorf("搜索结果按条交出（标题、网址、摘要）：%+v %v", results, err)
	}
}

// 高级档：搜索和读网页都用 advanced、偏向指定的国家；积分按高级档记（搜索一次 2 积分，读网页每 5 页 2 积分）。
func TestTavilyAdvancedOptions(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var lastQuery atomic.Value
	server, _ := fakeTavily(t, "tvly-test", &lastQuery)
	defer server.Close()
	searcher := NewTavilySearcherWith(server.URL, "tvly-test", TavilyOptions{Advanced: true, Country: "japan"}, nil, logger)
	defer searcher.Close()
	meter := &countingMeter{}

	if _, err := searcher.Search(ctx, "日经平均", 5, meter); err != nil {
		t.Fatal(err)
	}
	if got := lastQuery.Load(); got != "日经平均|5|advanced|false|japan" {
		t.Errorf("高级档搜索要带 advanced 和国家：%v", got)
	}
	if _, err := searcher.ReadPages(ctx, []string{"https://example.com/a"}, 4000, "要找的内容", meter); err != nil {
		t.Fatal(err)
	}
	if got := lastQuery.Load(); got != "extract:要找的内容|advanced" {
		t.Errorf("高级档读网页要带 advanced：%v", got)
	}
	if got := meter.all(); !slices.Equal(got, []searchCharge{{credits: 2}, {credits: 2}}) {
		t.Errorf("高级档搜索一次、读一页各记 2 积分：%+v", got)
	}
	if searcher.SearchCredits() != 2 || searcher.ReadCredits(5) != 2 || searcher.ReadCredits(6) != 4 {
		t.Error("高级档：搜索 2 积分，读网页每 5 页 2 积分")
	}
	// 阳性对照：零值的档位照旧是基础档、不带国家
	basic := NewTavilySearcher(server.URL, "tvly-test", nil, logger)
	defer basic.Close()
	if _, err := basic.Search(ctx, "日经平均", 3, meter); err != nil || lastQuery.Load() != "日经平均|3|basic|false" {
		t.Errorf("零值的档位应是基础档、不带国家：%v %v", lastQuery.Load(), err)
	}
}

// Tavily 的回复认不出（像服务改版了）时报错，不把认不出的内容当成结果；请求发出去了，照样记账。
func TestTavilyUnexpectedResponseIsAnError(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var lastQuery atomic.Value
	server, _ := fakeTavily(t, "tvly-test", &lastQuery)
	defer server.Close()
	searcher := NewTavilySearcher(server.URL, "tvly-test", nil, logger)
	defer searcher.Close()
	meter := &countingMeter{}
	if results, err := searcher.Search(ctx, oddQuery, 3, meter); !errors.Is(err, errUnexpectedResponse) || results != nil {
		t.Errorf("搜索结果认不出时应报错：%+v %v", results, err)
	}
	if pages, err := searcher.ReadPages(ctx, []string{oddURL}, 2000, "要找的内容", meter); !errors.Is(err, errUnexpectedResponse) || pages != nil {
		t.Errorf("读网页的结果认不出时应报错：%+v %v", pages, err)
	}
	if got := meter.all(); len(got) != 2 {
		t.Errorf("请求发出去了就记账：%+v", got)
	}
	// 阳性对照：同一个服务回约定的格式时搜得到
	if results, err := searcher.Search(ctx, "日经平均", 3, meter); err != nil || len(results) != 1 {
		t.Errorf("约定的格式应搜得到：%+v %v", results, err)
	}
}

// 发出去的每一次请求都记一笔，不管成没成功：会话断了重连再试时两次各记（断掉的那次标不完整），
// Tavily 说这次调用有错时照记（标不完整、不重试）。
func TestTavilyRecordsEveryRequestSent(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var lastQuery atomic.Value
	server, restart := fakeTavily(t, "tvly-test", &lastQuery)
	defer server.Close()
	searcher := NewTavilySearcher(server.URL, "tvly-test", nil, logger)
	defer searcher.Close()
	meter := &countingMeter{}

	if _, err := searcher.Search(ctx, "第一次", 3, meter); err != nil {
		t.Fatal(err)
	}
	restart() // Tavily 重启：旧会话没了，这次的第一个请求白发
	if _, err := searcher.Search(ctx, "重启之后", 3, meter); err != nil {
		t.Fatalf("会话断了应重连再试一次：%v", err)
	}
	if _, err := searcher.Search(ctx, badQuery, 3, meter); err == nil {
		t.Fatal("Tavily 说参数不对时应报错")
	}
	want := []searchCharge{{credits: 1}, {credits: 1, incomplete: true}, {credits: 1}, {credits: 1, incomplete: true}}
	if got := meter.all(); !slices.Equal(got, want) {
		t.Errorf("每发出一次请求记一笔（没拿到结果的标不完整）：%+v，应为 %+v", got, want)
	}
}

// 启动时不连；第一次用到时才连。Tavily 重启（会话没了）后：当次丢掉旧会话、重连再试一次，照样搜得到。
func TestTavilyConnectsLazilyAndReconnects(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var lastQuery atomic.Value
	var requests atomic.Int32
	fake, restart := fakeTavily(t, "tvly-test", &lastQuery)
	defer fake.Close()
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		proxy := fake.URL + r.URL.Path
		request, _ := http.NewRequestWithContext(r.Context(), r.Method, proxy, r.Body)
		request.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			w.Header()[key] = values
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer counting.Close()

	searcher := NewTavilySearcher(counting.URL, "tvly-test", nil, logger)
	defer searcher.Close()
	if requests.Load() != 0 {
		t.Fatal("创建时不应连 Tavily")
	}
	if _, err := searcher.Search(ctx, "第一次", 3, &countingMeter{}); err != nil {
		t.Fatalf("第一次用到时应连上并搜到：%v", err)
	}
	restart() // Tavily 重启：旧会话没了
	if _, err := searcher.Search(ctx, "重启之后", 3, &countingMeter{}); err != nil {
		t.Fatalf("会话断了应重连再试一次：%v", err)
	}
	if got := lastQuery.Load(); got != "重启之后|3|basic|false" {
		t.Errorf("重连后的搜索应到达服务：%v", got)
	}
}
