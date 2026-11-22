package research

import "context"

// 调查要用的外部能力，只要用到的方法：联网搜索、搜索的额度与记账、调查的额度。
// 由装配处接上（cmd/haro-server：搜索是 internal/tools 的 Tavily，额度与记账是 internal/spending）。

// SearchHit 是搜索结果里的一条。
type SearchHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Page 是读到的一个网页：Content 是正文（已截到每页的上限）；没读到时 Error 写原因，没有正文。
type Page struct {
	URL     string `json:"url"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Searcher 是联网搜索。实现每真的发出一次请求（重试时每次各算）就用 record 记一笔，不管成没成功：
// 请求到了服务商那边就可能已经扣了积分；只有实现知道一次调用发出了几次请求。
type Searcher interface {
	// Search 搜一次，返回几条结果。
	Search(ctx context.Context, query string, maxResults int, record Recorder) ([]SearchHit, error)
	// ReadPages 读几个网页的正文，每页最多 maxRunesPerPage 字；focus（调查的问题）不空时优先给出和它相关的段落。
	ReadPages(ctx context.Context, urls []string, maxRunesPerPage int, focus string, record Recorder) ([]Page, error)
	// SearchCredits / ReadCredits 是一次请求要用的积分（查额度用）。
	SearchCredits() int
	ReadCredits(pages int) int
}

// Recorder 记下发出去的一次搜索请求：credits 是这次要用的积分；incomplete = 没拿到结果（出错、被打断）。
type Recorder interface {
	RecordSearch(ctx context.Context, credits int, incomplete bool) error
}

// Meter 管调查的搜索额度与记账（按调查的用途）。
type Meter interface {
	AllowSearch(ctx context.Context, credits int) error
	Recorder
}

// Budget 是调查的额度：派任务前、每一轮之前都查一次。
type Budget interface {
	AllowResearch(ctx context.Context) error
}
