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
// 记账由实现用调用方给的 record 记：每真的发出一次请求（重试时每次各算）记一笔，不管成没成功——
// 请求到了服务商那边就可能已经扣了积分；没拿到结果的标不完整。只有实现知道一次调用发出了几次请求。
type WebSearcher interface {
	// Search 搜一次，返回几条结果（标题、网址、摘要）。
	Search(ctx context.Context, query string, maxResults int, record SearchRecorder) ([]SearchResult, error)
	// ReadPages 抓几个网页的正文；每页最多 maxRunesPerPage 字（R2：读得越多 token 越贵）。读不到的那页在结果里写原因。
	// focus 是要找的内容（调查的问题）：不空时优先给出和它相关的段落，而不是网页开头（长网页、PDF 很有用）。
	ReadPages(ctx context.Context, urls []string, maxRunesPerPage int, focus string, record SearchRecorder) ([]PageContent, error)
	// SearchCredits / ReadCredits 是一次请求要用的积分（记账、查额度用）。
	SearchCredits() int
	ReadCredits(pages int) int
}

// SearchResult 是搜索结果里的一条：只留标题、网址、摘要（request_id、图片等杂项不要，省 token）。
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// PageContent 是读到的一个网页：Content 是正文（已截到每页的上限）；读不到时 Error 写原因，没有正文。
type PageContent struct {
	URL     string `json:"url"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// errUnexpectedResponse 表示 Tavily 的回复不是约定的格式（服务改版了？）：报错，不把认不出的内容当成结果。
var errUnexpectedResponse = errors.New("Tavily 的回复格式不对（服务改版了？）")

// SearchRecorder 记下发出去的一次搜索请求（由 spending 实现）。
type SearchRecorder interface {
	// RecordSearch 记一笔：credits 是这次请求要用的积分；incomplete = 这次没拿到结果（出错、被打断：服务商扣没扣不知道，
	// 照记、标不完整）。ctx 被取消了也要记上（实现自己不跟着取消）。
	RecordSearch(ctx context.Context, credits int, incomplete bool) error
}

// SearchMeter 管搜索的额度与记账（由 spending 实现；按用途分开：快查 / 后台调查）。
type SearchMeter interface {
	AllowSearch(ctx context.Context, credits int) error
	SearchRecorder
}

// TavilyName 是联网搜索服务商的名字（MCP 连接的名字，也是记账时这笔花费的名目）。
const TavilyName = "tavily"

// TavilySearcher 经 Tavily 官方远程 MCP（https://mcp.tavily.com/mcp/）搜索。
// 只用 tavily_search / tavily_extract 两个工具；crawl / map / research 不用（贵、慢，调查流程由服务端自己控制）。
// 第一次用到时才连；会话断了（Tavily 重启、网络断过）丢掉旧的，当次重连再试一次，之后用到时照样重连。
type TavilySearcher struct {
	spec    ServerSpec
	options TavilyOptions
	logger  *slog.Logger

	mu     sync.Mutex
	server *MCPServer // nil = 还没连上，或者断了
}

// TavilyOptions 是 Tavily 的档位。零值是线上在用的：基础档、不偏向哪个国家。
type TavilyOptions struct {
	// Advanced：搜索和读网页都用高级档（相关度更高、读网页能读表格和嵌入内容；慢一些、积分翻倍）
	Advanced bool
	// Country：偏向这个国家的结果（Tavily 的写法：英文小写国名，例如 "japan"；只是加权，不是过滤）；空 = 不偏向
	Country string
}

// Tavily 的积分规则（官方文档）：搜索一次基础档 1 积分、高级档 2 积分；
// 读网页每 5 个网页基础档 1 积分、高级档 2 积分（读不到的不算，这里按请求的页数记，宁可记多）。
const (
	tavilyBasicCredits    = 1
	tavilyAdvancedCredits = 2
	tavilyPagesPerCharge  = 5
)

// NewTavilySearcher 准备经 Tavily 远程 MCP 搜索（这时不连）；密钥放在 Authorization 请求头里（不放网址，免得进日志）。
// 用线上的档位（TavilyOptions 的零值）。
func NewTavilySearcher(endpoint, apiKey string, client *http.Client, logger *slog.Logger) *TavilySearcher {
	return NewTavilySearcherWith(endpoint, apiKey, TavilyOptions{}, client, logger)
}

// NewTavilySearcherWith 同 NewTavilySearcher，按 options 的档位搜（比较不同档位时用）。
func NewTavilySearcherWith(endpoint, apiKey string, options TavilyOptions, client *http.Client, logger *slog.Logger) *TavilySearcher {
	return &TavilySearcher{logger: logger, options: options, spec: ServerSpec{Name: TavilyName, URL: endpoint, Client: client,
		Headers: map[string]string{"Authorization": "Bearer " + apiKey}}}
}

// depth 是 Tavily 的 search_depth / extract_depth。
func (t *TavilySearcher) depth() string {
	if t.options.Advanced {
		return "advanced"
	}
	return "basic"
}

// creditsPerCharge 是搜索一次（或读每 5 个网页）的积分。
func (t *TavilySearcher) creditsPerCharge() int {
	if t.options.Advanced {
		return tavilyAdvancedCredits
	}
	return tavilyBasicCredits
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
// 每发出一次请求用 record 记一笔 credits 积分（没拿到结果的标不完整）；连都没连上时请求没发出去，不记。
func (t *TavilySearcher) call(ctx context.Context, name string, arguments map[string]any, credits int, record SearchRecorder) (string, error) {
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
		// 写不进时由计量那边留着补写，补写成功之前不放行有上限的搜索（spending.WithBookkeeping）
		if recordErr := record.RecordSearch(ctx, credits, err != nil); recordErr != nil {
			t.logger.Warn("搜索用量没写进账本", "err", recordErr, "credits", credits, "incomplete", err != nil)
		}
		var toolErr *ToolError
		if err != nil && !errors.As(err, &toolErr) && ctx.Err() == nil {
			t.drop(server)
		}
		return text, err
	})
}

// Search 按档位搜、不带网页全文（快）。Tavily 的回复认不出（不是约定的 JSON、没有 results）时报错。
func (t *TavilySearcher) Search(ctx context.Context, query string, maxResults int, record SearchRecorder) ([]SearchResult, error) {
	arguments := map[string]any{"query": query, "max_results": maxResults, "search_depth": t.depth(), "include_raw_content": false}
	if t.options.Country != "" {
		arguments["country"] = t.options.Country
	}
	text, err := t.call(ctx, "tavily_search", arguments, t.SearchCredits(), record)
	if err != nil {
		return nil, err
	}
	var response struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &response); err != nil || response.Results == nil {
		return nil, fmt.Errorf("搜索：%w（%v）", errUnexpectedResponse, err)
	}
	results := make([]SearchResult, 0, len(response.Results))
	for _, found := range response.Results {
		results = append(results, SearchResult{Title: found.Title, URL: found.URL, Snippet: found.Content})
	}
	return results, nil
}

// ReadPages 按档位抓正文，每页单独截到 maxRunesPerPage 字（不会因为第一页太长把后面的挤掉）；没读到的那页带上原因。
// focus 交给 Tavily 的 query 参数，按相关度挑段落（2026-11-17 实测：21 页的考场空席 PDF，只取开头 2,000 字只看得到一个考场）。
// Tavily 的回复认不出时报错。
func (t *TavilySearcher) ReadPages(ctx context.Context, urls []string, maxRunesPerPage int, focus string, record SearchRecorder) ([]PageContent, error) {
	arguments := map[string]any{"urls": urls, "extract_depth": t.depth()}
	if focus != "" {
		arguments["query"] = focus
	}
	text, err := t.call(ctx, "tavily_extract", arguments, t.ReadCredits(len(urls)), record)
	if err != nil {
		return nil, err
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
	if err := json.Unmarshal([]byte(text), &response); err != nil || response.Results == nil {
		return nil, fmt.Errorf("读网页：%w（%v）", errUnexpectedResponse, err)
	}
	pages := make([]PageContent, 0, len(urls))
	for _, read := range response.Results {
		pages = append(pages, PageContent{URL: read.URL, Content: truncateRunes(read.RawContent, maxRunesPerPage)})
	}
	for _, failed := range response.Failed {
		pages = append(pages, PageContent{URL: failed.URL, Error: failed.Error})
	}
	return pages, nil
}

func (t *TavilySearcher) SearchCredits() int { return t.creditsPerCharge() }

func (t *TavilySearcher) ReadCredits(pages int) int {
	return (pages + tavilyPagesPerCharge - 1) / tavilyPagesPerCharge * t.creditsPerCharge()
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

// WebSearchLimits 是快查和读网页给模型多少：搜一次几条、搜索结果截到多少字、读网页每页多少字（读得越多这一轮越贵、越慢）。
type WebSearchLimits struct {
	Results     int // 搜一次最多几条
	ResultRunes int // 一次搜索的结果（编成 JSON 后）最多多少字
	PageRunes   int // 读网页每页最多多少字（有 focus 时优先给相关的段落）
}

// DefaultWebSearchLimits 是线上在用的：快查（R1）一次最多 3 条、截到 3000 字（够模型回答，又不至于每轮都贵）；
// 读网页每页 2000 字（和后台调查同一个数：之后每一轮都要带着它）。
func DefaultWebSearchLimits() WebSearchLimits {
	return WebSearchLimits{Results: 3, ResultRunes: 3000, PageRunes: 2000}
}

func (l WebSearchLimits) validate() error {
	if l.Results <= 0 || l.ResultRunes <= 0 || l.PageRunes <= 0 {
		return fmt.Errorf("快查与读网页的上限都要大于 0：%+v", l)
	}
	return nil
}

// WebSearchTool 是主对话里快查工具的名字（别的工具的说明里提到它时引用这里）。
const WebSearchTool = "web_search"

// WebSearchTools 是主对话里的快查（web_search）和读网页（read_web_pages，web_read.go）：先查额度，发出去的请求都记账
// （见 QuickSearch）；两个工具共用每天的搜索积分。上限用 DefaultWebSearchLimits。
func WebSearchTools(searcher WebSearcher, meter SearchMeter, logger *slog.Logger) Builtin {
	return webSearchTools(searcher, meter, DefaultWebSearchLimits(), logger)
}

// WebSearchToolsWithLimits 同 WebSearchTools，按 limits 给（比较不同上限时用）；上限不合法时报错。
func WebSearchToolsWithLimits(searcher WebSearcher, meter SearchMeter, limits WebSearchLimits, logger *slog.Logger) (Builtin, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return webSearchTools(searcher, meter, limits, logger), nil
}

func webSearchTools(searcher WebSearcher, meter SearchMeter, limits WebSearchLimits, logger *slog.Logger) Builtin {
	return func(server *mcp.Server) {
		webReadTool(server, searcher, meter, limits.PageRunes, logger)
		AddTextTool(server, &mcp.Tool{Name: WebSearchTool, Annotations: ReadOnly,
			Description: "联网搜索一次，查实时或最新的事实（新闻、价格、比分、营业时间、某件事的最新进展等）。" +
				"你不确定、或者信息可能过时的时候用它，不要凭记忆回答。搜一次就能答的问题用它。" +
				"主人的话是语音识别出来的，地名、人名可能被写成读音相近的字：搜不到时换成读音相近的写法再搜一次。" +
				"摘要里没有具体内容（时刻、价格、营业时间）时，用 " + ReadWebPagesTool + " 读结果里的页面。" +
				"没搜到想要的，就简短说没查到，再给一个下一步（例如派后台调查），不要长篇猜测。"},
			func(ctx context.Context, in webSearchInput) (string, error) {
				if strings.TrimSpace(in.Query) == "" {
					return "", errors.New("query 不能为空")
				}
				return quickSearchWith(ctx, searcher, meter, in.Query, limits, logger)
			})
	}
}

type webSearchInput struct {
	Query string `json:"query" jsonschema:"搜索词，简洁具体，例如「日经平均 今日 終値」"`
}

// QuickSearch 是一次快查（按 DefaultWebSearchLimits：最多几条、编成 JSON 后截到多少字）：先查额度再搜；
// 发出去的每次请求不管成没成功都记账（搜索那一层记，见 WebSearcher）。
// 主对话的 web_search 和看画面的专家都用它，额度算在同一处。
func QuickSearch(ctx context.Context, searcher WebSearcher, meter SearchMeter, query string, logger *slog.Logger) (string, error) {
	return quickSearchWith(ctx, searcher, meter, query, DefaultWebSearchLimits(), logger)
}

// quickSearchWith 同 QuickSearch，按 limits 的条数与字数。
func quickSearchWith(ctx context.Context, searcher WebSearcher, meter SearchMeter, query string, limits WebSearchLimits,
	logger *slog.Logger) (string, error) {
	if err := meter.AllowSearch(ctx, searcher.SearchCredits()); err != nil {
		return "", err
	}
	results, err := searcher.Search(ctx, query, limits.Results, meter)
	if err != nil {
		logger.Info("联网搜索", "query", query, "err", err)
		return "", err
	}
	data, err := json.Marshal(quickSearchOutput{Results: results})
	if err != nil {
		return "", err
	}
	text := string(data)
	logger.Info("联网搜索", "query", query, "results", len(results), "chars", utf8.RuneCountInString(text))
	return truncateRunes(text, limits.ResultRunes), nil
}

// quickSearchOutput 是快查交给模型的结果。
type quickSearchOutput struct {
	Results []SearchResult `json:"results"`
}

// truncateRunes 截到 limit 个字（按字符，不切坏中文）。
func truncateRunes(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…（后面省略）"
}
