package main

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/spending"
	"github.com/bukahou/haro/internal/tools"
)

// 后台调查的装配：任务账本、调查用的模型、上限，接上联网搜索（internal/tools）与额度、记账（internal/spending）。

// startResearch 开启后台调查：任务账本、调查用的模型、上限，并在后台开始处理队列。
func startResearch(ctx context.Context, settings config.ServerSettings, deps services, searcher tools.WebSearcher,
	logger *slog.Logger) (*research.Runner, func() error, error) {
	store, err := research.OpenTaskStore(ctx, settings.ResearchDB)
	if err != nil {
		return nil, nil, err
	}
	if settings.ResearchDB == "" {
		logger.Warn("没设 HARO_RESEARCH_DB：调查任务只记在内存里，服务端重启后就忘了")
	}
	// 调查在后台、不急：限流 / 服务端一时出错时让 SDK 重试两次；每次请求不另设期限，受调查自己的总时限管
	model := deps.meteredCloudModel(settings, cloudModelSpec{model: settings.CloudLLMModel, maxTokens: researchMaxTokens,
		effort: settings.ResearchEffort, thinking: settings.CloudLLMThinking, retries: 2}, spending.PurposeResearch, logger)
	limits := researchLimits(settings)
	meter := spending.NewSearchMeter(deps.guard, deps.ledger, deps.pricing, spending.PurposeResearch, tools.TavilyName)
	runner := research.NewRunner(store, model, researchSearch{searcher: searcher}, meter, deps.guard, limits, logger)
	writer := "（调查模型自己写）"
	if settings.StrongLLMFor(config.StrongForResearch) { // 搜索和读网页用便宜的模型，结论交给更强的模型写
		runner.WriteConclusionsWith(deps.meteredCloudModel(settings, cloudModelSpec{model: settings.StrongLLMModel,
			maxTokens: researchConclusionMaxTokens, effort: settings.ResearchEffort, thinking: settings.StrongLLMThinking, retries: 2},
			spending.PurposeResearch, logger))
		writer = settings.StrongLLMModel
	}
	running := make(chan struct{})
	go func() {
		defer close(running)
		runner.Run(ctx) // ctx 随退出信号结束：手上的调查停下（下次启动标成被中断）
	}()
	logger.Info("后台调查已开启", "model", settings.CloudLLMModel, "conclusion_writer", writer, "timeout", limits.Timeout,
		"max_rounds", limits.MaxRounds, "max_searches", limits.MaxSearches, "max_tokens", limits.MaxTokens)
	stop := func() error { // 先等调查的循环退出，再关任务账本：不然它最后一次写任务时库已经关了
		select {
		case <-running:
		case <-time.After(shutdownResearch):
			logger.Warn("后台调查没在期限内停下：照样关任务账本", "timeout", shutdownResearch)
		}
		return store.Close()
	}
	return runner, stop, nil
}

// researchMaxTokens 是调查时每次请求的输出上限（结论比聊天长，聊天用 HARO_CLOUD_LLM_MAX_TOKENS）。
const researchMaxTokens = 1000

// researchConclusionMaxTokens 是更强的模型写结论时的输出上限：它开着思考（思考也算在输出上限里），结论连同来源
// 经 submit_conclusion 一次交出，按 researchMaxTokens 会写到一半被截断（实测），这次调用白花。只按实际用的计费，
// 每天的调查额度照样管着。
const researchConclusionMaxTokens = 4000

// researchLimits 是每个调查任务的上限：配置里设了的用配置的，没设（0）的用程序里的默认值（research.DefaultLimits，默认值只在那里写）。
func researchLimits(settings config.ServerSettings) research.Limits {
	limits := research.DefaultLimits
	limits.Timeout = cmp.Or(settings.ResearchTimeout, limits.Timeout)
	limits.MaxRounds = cmp.Or(settings.ResearchMaxRounds, limits.MaxRounds)
	limits.MaxSearches = cmp.Or(settings.ResearchMaxSearches, limits.MaxSearches)
	limits.MaxTokens = cmp.Or(settings.ResearchMaxTokens, limits.MaxTokens)
	return limits
}

// researchSearch 把联网搜索（tools.WebSearcher）接成调查要的样子（research.Searcher）：调用照转，结果换成调查的类型。
type researchSearch struct {
	searcher tools.WebSearcher
}

func (s researchSearch) Search(ctx context.Context, query string, maxResults int, record research.Recorder) ([]research.SearchHit, error) {
	results, err := s.searcher.Search(ctx, query, maxResults, record)
	if err != nil {
		return nil, err
	}
	hits := make([]research.SearchHit, len(results))
	for i, result := range results {
		hits[i] = research.SearchHit(result)
	}
	return hits, nil
}

func (s researchSearch) ReadPages(ctx context.Context, urls []string, maxRunesPerPage int, focus string,
	record research.Recorder) ([]research.Page, error) {
	contents, err := s.searcher.ReadPages(ctx, urls, maxRunesPerPage, focus, record)
	if err != nil {
		return nil, err
	}
	pages := make([]research.Page, len(contents))
	for i, content := range contents {
		pages[i] = research.Page(content)
	}
	return pages, nil
}

func (s researchSearch) SearchCredits() int { return s.searcher.SearchCredits() }

func (s researchSearch) ReadCredits(pages int) int { return s.searcher.ReadCredits(pages) }
