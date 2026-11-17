package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/brain"
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
type TavilySearcher struct {
	server *MCPServer
}

// Tavily 的积分规则（官方文档）：基础搜索 1 积分；基础抓取每 5 个网页 1 积分。
const (
	tavilySearchCredits  = 1
	tavilyPagesPerCredit = 5
)

// ConnectTavily 连接 Tavily 远程 MCP；密钥放在 Authorization 请求头里（不放网址，免得进日志）。
func ConnectTavily(ctx context.Context, endpoint, apiKey string, client *http.Client, logger *slog.Logger) (*TavilySearcher, error) {
	server, err := ConnectMCPServer(ctx, ServerSpec{Name: "tavily", URL: endpoint, Client: client,
		Headers: map[string]string{"Authorization": "Bearer " + apiKey}}, logger)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"tavily_search", "tavily_extract"} {
		if server.Tool(name) == nil {
			server.Close()
			return nil, fmt.Errorf("Tavily 的工具清单里没有 %s（服务改版了？）", name)
		}
	}
	return &TavilySearcher{server: server}, nil
}

// Search 基础深度、不带网页全文（快）。只留标题、网址、摘要给模型（request_id、图片等杂项去掉，省 token）。
func (t *TavilySearcher) Search(ctx context.Context, query string, maxResults int) (string, error) {
	text, err := t.server.CallKnown(ctx, "tavily_search", map[string]any{
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
	text, err := t.server.CallKnown(ctx, "tavily_extract", arguments)
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

func (t *TavilySearcher) SearchCredits() int { return tavilySearchCredits }

func (t *TavilySearcher) ReadCredits(pages int) int {
	return (pages + tavilyPagesPerCredit - 1) / tavilyPagesPerCredit
}

// Close 断开。
func (t *TavilySearcher) Close() error { return t.server.Close() }

// 快查（R1）：一次搜索、最多 3 条，当场回答。结果截到这个长度（够模型回答，又不至于每轮都贵）。
const (
	quickSearchResults  = 3
	quickSearchMaxRunes = 3000
)

// WebSearchTools 返回主对话的快查工具 web_search（R1）。
func WebSearchTools(searcher WebSearcher, meter SearchMeter, logger *slog.Logger) []Builtin {
	return []Builtin{{
		Spec: brain.ToolSpec{Name: "web_search",
			Description: "联网搜索一次，查实时或最新的事实（新闻、价格、比分、营业时间、某件事的最新进展等）。" +
				"你不确定、或者信息可能过时的时候用它，不要凭记忆回答。搜一次就能答的问题用它。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"搜索词，简洁具体，例如「日经平均 今日 终値」"}},"required":["query"]}`)},
		Run: func(ctx context.Context, arguments map[string]any) (string, error) {
			var in struct {
				Query string `json:"query"`
			}
			if err := decodeArguments(arguments, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Query) == "" {
				return "", errors.New("query 不能为空")
			}
			return meteredSearch(ctx, searcher, meter, in.Query, quickSearchResults, quickSearchMaxRunes, logger)
		},
	}}
}

// meteredSearch 先查额度、搜索、成功后记账。
func meteredSearch(ctx context.Context, searcher WebSearcher, meter SearchMeter, query string, maxResults, maxRunes int,
	logger *slog.Logger) (string, error) {
	credits := searcher.SearchCredits()
	if err := meter.AllowSearch(ctx, credits); err != nil {
		return "", err
	}
	text, err := searcher.Search(ctx, query, maxResults)
	logger.Info("联网搜索", "query", query, "chars", utf8.RuneCountInString(text), "err", err)
	if err != nil {
		return "", err
	}
	if err := meter.RecordSearch(context.WithoutCancel(ctx), credits); err != nil {
		logger.Warn("搜索用量没记上", "err", err)
	}
	return truncateRunes(text, maxRunes), nil
}

// truncateRunes 截到 limit 个字（按字符，不切坏中文）。
func truncateRunes(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…（后面省略）"
}
