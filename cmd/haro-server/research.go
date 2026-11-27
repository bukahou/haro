package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/spending"
	"github.com/bukahou/haro/internal/tools"
)

// 后台调查的装配：任务账本、调查和审查的代理（按定义，见 agentAssembly.researchAgents）、上限，接上联网搜索（internal/tools）
// 与额度、记账（internal/spending）。

// startResearch 开启后台调查：先按定义装配好代理（写错了就起不来），再开任务账本，在后台开始处理队列。
func startResearch(ctx context.Context, settings config.ServerSettings, assembly agentAssembly, searcher tools.WebSearcher,
	logger *slog.Logger) (*research.Runner, func() error, error) {
	limits := researchLimits(settings)
	built, err := assembly.researchAgents(limits)
	if err != nil {
		return nil, nil, err
	}
	store, err := research.OpenTaskStore(ctx, settings.ResearchDB)
	if err != nil {
		return nil, nil, err
	}
	if settings.ResearchDB == "" {
		logger.Warn("没设 HARO_RESEARCH_DB：调查任务只记在内存里，服务端重启后就忘了")
	}
	meter := spending.NewSearchMeter(assembly.guard, assembly.ledger, assembly.pricing, spending.PurposeResearch, tools.TavilyName)
	runner, err := research.NewRunner(store, built, researchSearch{searcher: searcher}, meter, assembly.guard, limits, logger)
	if err != nil {
		return nil, nil, errors.Join(err, store.Close())
	}
	running := make(chan struct{})
	go func() {
		defer close(running)
		runner.Run(ctx) // ctx 随退出信号结束：手上的调查停下（下次启动标成被中断）
	}()
	logger.Info("后台调查已开启", "timeout", limits.Timeout, "max_rounds", built.Investigator.MaxSteps,
		"max_searches", limits.MaxSearches, "max_tokens", limits.MaxTokens, "reviewed", built.Reviewer != nil,
		"longest_wait", runner.LongestWait())
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

// researchLimits 是每个调查任务的上限：配置里设了的用配置的，没设（0）的用程序里的默认值（research.DefaultLimits，默认值只在
// 那里写）。轮数不在这里：是调查的代理的步数（见 agentAssembly.researchAgents）。
func researchLimits(settings config.ServerSettings) research.Limits {
	limits := research.DefaultLimits
	limits.Timeout = cmp.Or(settings.ResearchTimeout, limits.Timeout)
	limits.MaxSearches = cmp.Or(settings.ResearchMaxSearches, limits.MaxSearches)
	limits.MaxTokens = cmp.Or(settings.ResearchMaxTokens, limits.MaxTokens)
	limits.MaxCallTokens = cmp.Or(settings.ResearchMaxCallTokens, limits.MaxCallTokens)
	limits.WriteUpTimeout = cmp.Or(settings.ResearchWriteUpTimeout, limits.WriteUpTimeout)
	limits.RecheckTimeout = cmp.Or(settings.ResearchRecheckTimeout, limits.RecheckTimeout)
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
