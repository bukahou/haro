package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// WebSearcher 是联网搜索的能力（v5 E3）：主对话的快查（R1）与后台调查共用。
// 现在的实现是 Tavily（经它的官方远程 MCP）；换 Exa 等只要新写一个实现。
type WebSearcher interface {
	// Search 搜一次，返回给模型看的结果文字（标题、网址、摘要）。
	Search(ctx context.Context, query string, maxResults int) (string, error)
	// ReadPages 抓几个网页的正文；每页最多 maxRunesPerPage 字（R2：读得越多 token 越贵）。
	// focus 是要找的内容（调查的问题）：不空时优先给出和它相关的段落，而不是网页开头（长网页、PDF 很有用）。
	ReadPages(ctx context.Context, urls []string, maxRunesPerPage int, focus string) (string, error)
	// SearchCredits / ReadCredits 是一次调用要用的积分（记账、查额度用）。
	SearchCredits() int
	ReadCredits(pages int) int
}

// SearchMeter 管搜索的额度与记账（由 spending 实现；按用途分开：快查 / 后台调查）。
type SearchMeter interface {
	AllowSearch(ctx context.Context, credits int) error
	RecordSearch(ctx context.Context, credits int) error
}

// TavilySearcher 经 Tavily 官方远程 MCP（https://mcp.tavily.com/mcp/）搜索。
// 只用 tavily_search / tavily_extract 两个工具；crawl / map / research 不用（贵、慢，调查流程由服务端自己控制）。
// 第一次用到时才连；会话断了（Tavily 重启、网络断过）丢掉旧的，当次重连再试一次，之后用到时照样重连。
type TavilySearcher struct {
	spec   ServerSpec
	logger *slog.Logger

	mu     sync.Mutex
	server *MCPServer // nil = 还没连上，或者断了
}

// Tavily 的积分规则（官方文档）：基础搜索 1 积分；基础抓取每 5 个网页 1 积分。
const (
	tavilySearchCredits  = 1
	tavilyPagesPerCredit = 5
)

// NewTavilySearcher 准备经 Tavily 远程 MCP 搜索（这时不连）；密钥放在 Authorization 请求头里（不放网址，免得进日志）。
func NewTavilySearcher(endpoint, apiKey string, client *http.Client, logger *slog.Logger) *TavilySearcher {
	return &TavilySearcher{logger: logger, spec: ServerSpec{Name: "tavily", URL: endpoint, Client: client,
		Headers: map[string]string{"Authorization": "Bearer " + apiKey}}}
}

// connected 返回连着的会话；还没连就连上（并核对要用的两个工具都在）。
func (t *TavilySearcher) connected(ctx context.Context) (*MCPServer, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.server != nil {
		return t.server, nil
	}
	server, err := ConnectMCPServer(ctx, t.spec, t.logger)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"tavily_search", "tavily_extract"} {
		if server.Tool(name) == nil {
			server.Close()
			return nil, fmt.Errorf("Tavily 的工具清单里没有 %s（服务改版了？）", name)
		}
	}
	t.server = server
	return server, nil
}

// drop 丢掉断了的会话（下次用到时重连）。
func (t *TavilySearcher) drop(server *MCPServer) {
	t.mu.Lock()
	if t.server == server {
		t.server = nil
	}
	t.mu.Unlock()
	_ = server.Close()
	t.logger.Warn("联网搜索：和 Tavily 的会话断了，下次用到时重连")
}

// call 调用一个 Tavily 工具。连接出问题（不是 Tavily 说这次调用有错）时丢掉会话、重连再试一次。
func (t *TavilySearcher) call(ctx context.Context, name string, arguments map[string]any) (string, error) {
	retry := retrypolicy.NewBuilder[string]().WithMaxRetries(1).
		HandleIf(func(_ string, err error) bool {
			var toolErr *ToolError
			return err != nil && !errors.As(err, &toolErr) && ctx.Err() == nil
		}).Build()
	return failsafe.With(retry).WithContext(ctx).Get(func() (string, error) {
		server, err := t.connected(ctx)
		if err != nil {
			return "", fmt.Errorf("连不上 Tavily: %w", err)
		}
		text, err := server.CallKnown(ctx, name, arguments)
		var toolErr *ToolError
		if err != nil && !errors.As(err, &toolErr) && ctx.Err() == nil {
			t.drop(server)
		}
		return text, err
	})
}

// Search 基础深度、不带网页全文（快）。只留标题、网址、摘要给模型（request_id、图片等杂项去掉，省 token）。
func (t *TavilySearcher) Search(ctx context.Context, query string, maxResults int) (string, error) {
	text, err := t.call(ctx, "tavily_search", map[string]any{
		"query": query, "max_results": maxResults, "search_depth": "basic", "include_raw_content": false})
	if err != nil {
		return "", err
	}
	var response struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if json.Unmarshal([]byte(text), &response) != nil || response.Results == nil {
		return text, nil // 格式变了：原样交给模型
	}
	type result struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	}
	compact := make([]result, 0, len(response.Results))
	for _, found := range response.Results {
		compact = append(compact, result{Title: found.Title, URL: found.URL, Snippet: found.Content})
	}
	return marshalResult(map[string]any{"results": compact})
}

// ReadPages 基础深度抓正文，每页单独截到 maxRunesPerPage 字（不会因为第一页太长把后面的挤掉）。
// focus 交给 Tavily 的 query 参数，按相关度挑段落（2026-11-17 实测：21 页的考场空席 PDF，只取开头 2,000 字只看得到一个考场）。
func (t *TavilySearcher) ReadPages(ctx context.Context, urls []string, maxRunesPerPage int, focus string) (string, error) {
	arguments := map[string]any{"urls": urls, "extract_depth": "basic"}
	if focus != "" {
		arguments["query"] = focus
	}
	text, err := t.call(ctx, "tavily_extract", arguments)
	if err != nil {
		return "", err
	}
	var response struct {
		Results []struct {
			URL        string `json:"url"`
			RawContent string `json:"raw_content"`
		} `json:"results"`
		Failed []struct {
			URL   string `json:"url"`
			Error string `json:"error"`
		} `json:"failed_results"`
	}
	if json.Unmarshal([]byte(text), &response) != nil || response.Results == nil {
		return truncateRunes(text, maxRunesPerPage*len(urls)), nil // 格式变了：按总长度截
	}
	type page struct {
		URL     string `json:"url"`
		Content string `json:"content,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	pages := make([]page, 0, len(urls))
	for _, read := range response.Results {
		pages = append(pages, page{URL: read.URL, Content: truncateRunes(read.RawContent, maxRunesPerPage)})
	}
	for _, failed := range response.Failed {
		pages = append(pages, page{URL: failed.URL, Error: failed.Error})
	}
	return marshalResult(map[string]any{"pages": pages})
}

// marshalResult 把整理好的结果编成 JSON 文本（交给调查和快查）。
func marshalResult(value any) (string, error) {
	data, err := json.Marshal(value)
	return string(data), err
}

func (t *TavilySearcher) SearchCredits() int { return tavilySearchCredits }

func (t *TavilySearcher) ReadCredits(pages int) int {
	return (pages + tavilyPagesPerCredit - 1) / tavilyPagesPerCredit
}

// Close 断开。
func (t *TavilySearcher) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.server == nil {
		return nil
	}
	err := t.server.Close()
	t.server = nil
	return err
}

// 快查（R1）：一次搜索、最多 3 条，当场回答。结果截到这个长度（够模型回答，又不至于每轮都贵）。
const (
	quickSearchResults  = 3
	quickSearchMaxRunes = 3000
)

// WebSearchTools 是主对话里的快查（web_search）：先查额度、搜一次、成功后记账（见 QuickSearch）。
func WebSearchTools(searcher WebSearcher, meter SearchMeter, logger *slog.Logger) Builtin {
	return func(server *mcp.Server) {
		AddTextTool(server, &mcp.Tool{Name: "web_search", Annotations: ReadOnly,
			Description: "联网搜索一次，查实时或最新的事实（新闻、价格、比分、营业时间、某件事的最新进展等）。" +
				"你不确定、或者信息可能过时的时候用它，不要凭记忆回答。搜一次就能答的问题用它。"},
			func(ctx context.Context, in webSearchInput) (string, error) {
				if strings.TrimSpace(in.Query) == "" {
					return "", errors.New("query 不能为空")
				}
				return QuickSearch(ctx, searcher, meter, in.Query, logger)
			})
	}
}

type webSearchInput struct {
	Query string `json:"query" jsonschema:"搜索词，简洁具体，例如「日经平均 今日 終値」"`
}

// QuickSearch 是一次快查（最多 quickSearchResults 条、截到 quickSearchMaxRunes 字）：先查额度、搜索、成功后记账。
// 主对话的 web_search 和看画面的专家都用它，额度算在同一处。
func QuickSearch(ctx context.Context, searcher WebSearcher, meter SearchMeter, query string, logger *slog.Logger) (string, error) {
	credits := searcher.SearchCredits()
	if err := meter.AllowSearch(ctx, credits); err != nil {
		return "", err
	}
	text, err := searcher.Search(ctx, query, quickSearchResults)
	logger.Info("联网搜索", "query", query, "chars", utf8.RuneCountInString(text), "err", err)
	if err != nil {
		return "", err
	}
	if err := meter.RecordSearch(context.WithoutCancel(ctx), credits); err != nil {
		logger.Warn("搜索用量没记上", "err", err)
	}
	return truncateRunes(text, quickSearchMaxRunes), nil
}

// truncateRunes 截到 limit 个字（按字符，不切坏中文）。
func truncateRunes(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…（后面省略）"
}
