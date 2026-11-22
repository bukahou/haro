package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bukahou/haro/internal/brain"
)

// 调查的代理查资料的两个工具：搜索、读网页。先查额度再执行，拿到的资料按网址记下（审查时给摘录用）。

// 查资料时写死的几项（都不开放成配置）。
const (
	// searchResults 是调查时每次搜索要几条结果：多一条，之后每一轮都多带几百字的输入；5 条是对比四种调查做法的试验时
	// 用的数（Tavily 基础搜索不论几条都是 1 积分），改它要重新对比质量和花费。
	searchResults = 5
	// pageRunes 是读网页时每页最多给模型多少字（按调查的问题挑相关的段落）：读得越多，之后每一轮越贵；审查时每个来源给的
	// 摘录也是这么长（maxExcerptRunes：审查看到的就是调查时看到的）。2,000 字同样是试验时用的数，改它就同时改了调查和审查的
	// 花费与质量，要重新对比，不是部署时调的参数。
	pageRunes = 2000
	// maxPagesPerRead 是一次最多读几页：读得越多这一步越贵；多给的网址告诉模型没读，需要的话它再分批读。
	maxPagesPerRead = 3
)

// runTool 执行调查里的一次搜索或读网页，返回给模型看的文字（出错也作为文字交给模型，让它换个办法）。只执行调查的代理能用的
// 工具（没交给它的照着名字调用也不执行）；搜索最多搜到第 searchCap 次。成功拿到的才算资料（report.gathered），按网址记下
// （审查时给摘录用）；没成功的记下原因。
func (q *inquiry) runTool(ctx context.Context, call brain.ToolCall, searchCap int) string {
	r, report := q.r, &q.report
	if !allows(r.agents.Investigator, call.Name) {
		return "没有叫 " + call.Name + " 的工具"
	}
	switch call.Name {
	case searchWebTool:
		if report.searches >= searchCap {
			return fmt.Sprintf("搜索次数已经用完（上限 %d 次）。请根据已有资料写结论。", searchCap)
		}
		in, err := brain.DecodeArguments[searchInput](call.Arguments)
		if err != nil || strings.TrimSpace(in.Query) == "" {
			return failureText(errors.Join(errors.New("query 不能为空"), err))
		}
		var hits []SearchHit
		err = r.withinBudget(ctx, r.searcher.SearchCredits(), func() (err error) {
			hits, err = r.searcher.Search(ctx, in.Query, searchResults, r.meter)
			return err
		})
		report.searches++
		r.logger.Info("调查：搜索", "query", in.Query, "results", len(hits), "err", err)
		if err == nil {
			q.sources.addHits(hits)
		}
		return report.lookupResult(searchOutput{Results: hits}, err)
	case readPagesTool:
		in, err := brain.DecodeArguments[readPagesInput](call.Arguments)
		if err != nil || len(in.URLs) == 0 {
			return failureText(errors.Join(errors.New("urls 不能为空"), err))
		}
		urls, skipped := in.URLs, []string(nil)
		if len(urls) > maxPagesPerRead {
			urls, skipped = urls[:maxPagesPerRead], urls[maxPagesPerRead:]
		}
		var pages []Page
		err = r.withinBudget(ctx, r.searcher.ReadCredits(len(urls)), func() (err error) {
			pages, err = r.searcher.ReadPages(ctx, urls, pageRunes, q.question, r.meter)
			return err
		})
		report.reads++
		r.logger.Info("调查：读网页", "urls", len(urls), "skipped", len(skipped), "err", err)
		if err == nil {
			q.sources.addPages(pages)
		}
		out := readPagesOutput{Pages: pages}
		if len(skipped) > 0 { // 多给的网址没读：告诉模型是哪几个，需要的话它再分批读
			out.NotRead = skipped
			out.Note = fmt.Sprintf("一次最多读 %d 个网址，not_read 里的这次没读；需要的话再调用一次 %s 读它们。",
				maxPagesPerRead, readPagesTool)
		}
		return report.lookupResult(out, err)
	default:
		return "没有叫 " + call.Name + " 的工具"
	}
}

// searchOutput、readPagesOutput 是搜索、读网页交给模型的结果。
type searchOutput struct {
	Results []SearchHit `json:"results"`
}

type readPagesOutput struct {
	Pages   []Page   `json:"pages"`
	NotRead []string `json:"not_read,omitempty"` // 超过一次能读的个数、这次没读的网址
	Note    string   `json:"note,omitempty"`
}

// lookupResult 记下一次搜索或读网页的结果：成功的算一份资料，没成功的记下原因；返回给模型看的文字（结果编成 JSON）。
func (report *investigationReport) lookupResult(result any, err error) string {
	var text []byte
	if err == nil {
		text, err = json.Marshal(result)
	}
	if err != nil {
		report.lookupFailure = err.Error()
		return failureText(err)
	}
	report.gathered++
	return string(text)
}

// withinBudget 先查额度再执行。记账在搜索那一层：发出去的每次请求不管成没成功都记（run 里把 r.meter 交给它，见 Searcher）。
func (r *Runner) withinBudget(ctx context.Context, credits int, run func() error) error {
	if err := r.meter.AllowSearch(ctx, credits); err != nil {
		return err
	}
	return run()
}

type searchInput struct {
	Query string `json:"query" jsonschema:"搜索词，简洁具体"`
}

type readPagesInput struct {
	URLs []string `json:"urls" jsonschema:"要读的网址，取自搜索结果"`
}

// searchWebSpec、readPagesSpec 是调查的代理查资料的两个工具（名字见 agents.go）。
var (
	searchWebSpec = brain.ToolSpec{Name: searchWebTool, Description: "联网搜索，返回几条结果（标题、网址、摘要）。",
		Parameters: brain.ParametersFor[searchInput]()}
	readPagesSpec = brain.ToolSpec{Name: readPagesTool,
		Description: fmt.Sprintf("读网页正文（一次最多 %d 个网址；每页给出和调查的问题相关的段落）。摘要不够用时才读。", maxPagesPerRead),
		Parameters:  brain.ParametersFor[readPagesInput]()}
)
