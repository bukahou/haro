package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/backstory"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/embedding"
	"github.com/bukahou/haro/internal/graceful"
	"github.com/bukahou/haro/internal/household"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/memory"
	"github.com/bukahou/haro/internal/money"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/profile"
	"github.com/bukahou/haro/internal/readiness"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/speech"
	"github.com/bukahou/haro/internal/spending"
	"github.com/bukahou/haro/internal/telemetry"
	"github.com/bukahou/haro/internal/terminaltools"
	"github.com/bukahou/haro/internal/tools"
	"github.com/bukahou/haro/internal/vision"
	"github.com/bukahou/haro/internal/weather"
)

// 服务端按部件装配：外部服务与费用 → 工具 → 大脑 → 对外服务。每一步把关停时要收尾的事登记进 shutdownSteps。

// shutdownSteps 是关停时要依次收尾的事：后开的先关（和 defer 一样）。
type shutdownSteps []shutdownStep

// shutdownStep 是关停时的一步：name 是日志里写的名字。
type shutdownStep struct {
	name string
	run  func() error
}

func (s *shutdownSteps) add(name string, step func() error) {
	*s = append(*s, shutdownStep{name: name, run: step})
}

// run 依次收尾；出了错的各步不吞掉，最后合成一行警告（数据库的 WAL 没合并、遥测没发出去之类：数据还在，但要留痕）。
func (s shutdownSteps) run(logger *slog.Logger) {
	var problems []error
	for i := len(s) - 1; i >= 0; i-- {
		if err := s[i].run(); err != nil {
			problems = append(problems, fmt.Errorf("%s：%w", s[i].name, err))
		}
	}
	if len(problems) > 0 {
		logger.Warn("关停时有几步没做好", "steps", len(problems), "err", errors.Join(problems...))
	}
}

// startTelemetry 开启遥测；关停的最后一步在期限内（telemetry.ShutdownTimeout）把还没发出的发出去，采集器连不上时不卡住退出。
func startTelemetry(ctx context.Context, settings config.ServerSettings, shutdown *shutdownSteps) error {
	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.ServiceServer, settings.OTLPEndpoint)
	if err != nil {
		return err
	}
	shutdown.add("遥测", shutdownTelemetry)
	return nil
}

// services 是服务端连着的外部服务，以及费用的账本、额度、单价。
type services struct {
	httpClient *http.Client // 普通请求（带总超时）
	modelHTTP  *http.Client // 模型的流式请求（不带总超时：各用途按自己的期限）
	claude     *llm.ClaudeAPI
	synthesis  brain.SpeechSynthesizer
	streaming  brain.StreamingRecognizer
	ledger     spending.Ledger
	guard      *spending.Guard
	pricing    spending.Pricing
	// conversations 是对话库（没设 HARO_CONVERSATIONS_DB 时为 nil：对话只在内存里）：大脑存对话，长期记忆从完整记录里按意思找
	conversations *conversationstore.SQLiteStore
	embedder      *embedding.Client // 嵌入服务（没设 HARO_EMBEDDING_URL 时为 nil：长期记忆不按意思找）
	accounts      appAccounts       // App 接口的账号库和管理员（没开时为空）
}

// connectServices 准备外部服务（语音合成、识别、模型的客户端）和费用的刹车：先有刹车，再接搜索、后台调查。
func connectServices(ctx context.Context, settings config.ServerSettings, logger *slog.Logger, shutdown *shutdownSteps) (services, error) {
	deps := services{httpClient: &http.Client{Timeout: settings.RequestTimeout},
		// 模型的流式请求不用带总超时的客户端：客户端的总超时连读流也算在内，各用途要的长短不同 ⇒
		// 每个模型按用途给请求设期限（cloudModelSpec.requestTimeout；调查受它自己的总时限管）
		modelHTTP: &http.Client{}}
	// Claude 的客户端全进程只建一个，所有用途共用（见 llm.ClaudeAPI：多一个客户端 / HTTP 客户端就多一份令牌缓存，
	// 集群令牌只能换一次 ⇒ 互相把令牌用掉）。不用 Claude 时为 nil
	var err error
	if deps.claude, err = newClaudeAPI(settings, deps.modelHTTP); err != nil {
		return deps, err
	}
	if _, err := llm.ParseToolCallIDStyle(settings.CloudLLMToolCallIDs); err != nil {
		return deps, fmt.Errorf("HARO_CLOUD_LLM_TOOL_CALL_IDS: %w", err)
	}
	if deps.synthesis, err = buildSynthesizer(settings, deps.httpClient, logger); err != nil {
		return deps, err
	}
	deps.streaming = speech.NewRealtimeTranscriptionClient(settings.ASRURL)
	logger.Info("语音识别：haro-asr", "url", settings.ASRURL)

	ledger, err := spending.OpenLedger(ctx, settings.UsageDB)
	if err != nil {
		return deps, err
	}
	deps.ledger = spending.WithBookkeeping(ledger, logger)
	shutdown.add("用量账本", deps.ledger.Close)
	if settings.UsageDB == "" {
		logger.Warn("没设 HARO_USAGE_DB：用量只记在内存里，服务端重启后额度重新计算")
	}
	rates, err := exchangeRates(ctx, settings, deps.httpClient, logger)
	if err != nil {
		return deps, err
	}
	currency := settings.Currency()
	limits := spending.Limits{Currency: currency, ResearchDaily: money.Of(settings.ResearchDailyCap, currency),
		Monthly: money.Of(settings.MonthlyCap, currency), SearchDailyCredits: settings.SearchDailyCredits,
		WarnRatio: spending.DefaultWarnRatio, MonthlyRaise: money.Of(settings.MonthlyRaise, currency)}
	deps.guard = spending.NewGuard(deps.ledger, limits, rates, logger)
	logger.Info("费用上限（默认货币；账本按原始币种记，判断时按参考汇率换算）", "currency", currency,
		"research_daily", limits.ResearchDaily.String(), "monthly", limits.Monthly.String(), "search_daily_credits", settings.SearchDailyCredits)
	if deps.conversations, err = openConversationStore(ctx, settings, shutdown); err != nil {
		return deps, err
	}
	if settings.EmbeddingURL != "" {
		deps.embedder = embedding.NewClient(embedding.Options{BaseURL: settings.EmbeddingURL, Model: settings.EmbeddingModel,
			APIKey: settings.EmbeddingAPIKey, QueryPrefix: settings.EmbeddingQueryPrefix, DocumentPrefix: settings.EmbeddingDocumentPrefix})
		logger.Info("长期记忆按意思找：嵌入服务", "url", settings.EmbeddingURL, "model", settings.EmbeddingModel)
	} else {
		logger.Warn("没设 HARO_EMBEDDING_URL：长期记忆不按意思找（照样能记、列、忘、恢复；查只给最近记的，以前的对话查不了）")
	}
	deps.pricing, err = loadPricing(ctx, settings, deps.httpClient, logger)
	return deps, err
}

// tooling 是工具，以及每轮补充给模型的情况说明、给终端的提示。
type tooling struct {
	toolbox      brain.Toolbox            // 没开工具时为 nil
	briefers     []brain.Briefer          // 每轮补充给模型的即时情况：现在几号几点、额度提醒、主人家大致在哪、任务板
	notices      *brain.NoticeHub         // 给终端的提示（后台调查查完了让终端响一声）和服务端自己开口（到点的提醒）
	expertSearch vision.Search            // 看画面的专家联网查：和主对话的快查同一套搜索与额度；不能查时为 nil
	memory       *memory.Keeper           // 长期记忆（App 的概览数条数）；没开那组工具时为 nil
	research     *research.Runner         // 后台调查（App 的概览数进行中的）；没开时为 nil
	household    *household.Service       // 每个账号的主人资料、家的位置、节假日（大脑、天气、App 都从这里取）
	places       *weather.OpenMeteoClient // 搜地名（App 设家的位置用）；天气那组工具关着时为 nil
}

// buildTooling 准备工具：自研工具内置（长期记忆（记事已并进来）、提醒直接连数据库；天气；联网搜索与后台调查），MCP 只接第三方（现在还没有）。
// catalog 是代理定义（后台调查的代理按它装配）。
func buildTooling(ctx context.Context, settings config.ServerSettings, catalog agents.Catalog, deps services, logger *slog.Logger,
	shutdown *shutdownSteps) (tooling, error) {
	kit := tooling{briefers: []brain.Briefer{brain.ClockBriefer{}, deps.guard}, notices: brain.NewNoticeHub()}
	defaults := household.Defaults{Holidays: settings.Holidays}
	if settings.ProfileDir != "" { // 主人资料文件：管理员账号没在 App 里改过时用它，改了文件下一轮就生效
		defaults.Profiles = profile.OpenDir(settings.ProfileDir, logger)
		householdRunes, people := defaults.Profiles.Summary()
		logger.Info("主人资料文件", "dir", settings.ProfileDir, "household_runes", householdRunes, "people", people)
	}
	var weatherHTTP *http.Client
	if settings.ToolGroupOn(config.ToolGroupWeather) {
		weatherHTTP = weather.NewHTTPClient() // 连接等得短：连不上就早点交给备用
		defaults.Home = weatherHome(ctx, settings, weatherHTTP, logger)
	}
	var err error
	if kit.household, err = household.Open(ctx, householdStore(deps.accounts), deps.accounts.household(), defaults, logger); err != nil {
		return kit, err
	}
	if settings.Holidays != "" {
		logger.Info("节假日：今天、明天是节假日时告诉模型（没在 App 里设过的管理员账号用这个国家）", "country", settings.Holidays)
	}
	kit.briefers = append(kit.briefers, brain.BriefLines(kit.household.HolidayNote)) // 不是工具：关掉工具时照样有
	var builtins []tools.Builtin                                                     // 内置工具按组开（HARO_TOOLS，不设 = 全开）
	if settings.ToolGroupOn(config.ToolGroupMemory) {
		store, err := notes.Open(ctx, settings.NotesDB)
		if err != nil {
			return kit, err
		}
		shutdown.add("记事库", store.Close)
		keeper, stopMemory := startMemory(ctx, settings, store, deps, logger)
		kit.memory = keeper
		shutdown.add("长期记忆", stopMemory)
		builtins = append(builtins, tools.MemoryTools(keeper, time.Local, logger))
		kit.briefers = append(kit.briefers, keeper) // 常驻记忆：每轮附在情况说明里
	}
	if settings.ToolGroupOn(config.ToolGroupReminders) {
		scheduler, stopReminders, err := startReminders(ctx, settings, kit.notices, logger)
		if err != nil {
			return kit, err
		}
		shutdown.add("提醒", stopReminders)
		builtins = append(builtins, tools.ReminderTools(scheduler, logger))
		kit.briefers = append(kit.briefers, scheduler) // 错过的提醒：主人下次开口时告诉他
	}
	if settings.ToolGroupOn(config.ToolGroupWeather) { // 天气：Open-Meteo 为主（备用按 HARO_WEATHER_BACKUP）；都不用密钥
		openMeteo := weather.NewOpenMeteoClient(settings.WeatherForecastURL, settings.WeatherGeocodingURL, weatherHTTP)
		kit.places = openMeteo
		forecaster := weatherForecaster(ctx, settings, openMeteo, defaults.Home, weatherHTTP, logger)
		// 家按这一轮说话的人所在的账号取（kit.household）
		builtins = append(builtins, tools.WeatherTools(openMeteo, forecaster, kit.household, settings.WeatherGeocodingLanguage, logger))
		kit.briefers = append(kit.briefers, brain.BriefLines(weather.LocationNote{Home: kit.household}.Brief)) // 每轮告诉模型主人家大致在哪（城市级）
	}
	// 联网搜索服务：设了密钥就有。前台的搜索工具、后台调查各是一组；看画面的专家查资料也用它（不算前台的工具组）
	if searcher := connectWebSearch(settings, deps.httpClient, logger); searcher != nil {
		shutdown.add("联网搜索", searcher.Close)
		meter := spending.NewSearchMeter(deps.guard, deps.ledger, deps.pricing, spending.PurposeSearch, searcher.Name())
		kit.expertSearch = func(ctx context.Context, query string) (string, error) {
			return tools.QuickSearch(ctx, searcher, meter, query, logger)
		}
		if settings.ToolGroupOn(config.ToolGroupSearch) {
			builtins = append(builtins, tools.WebSearchTools(searcher, meter, logger))
		}
		if settings.ResearchesWithCloud() {
			runner, closeRunner, err := startResearch(ctx, settings, newAgentAssembly(settings, catalog, deps, logger), searcher, logger)
			if err != nil {
				return kit, err
			}
			kit.research = runner
			shutdown.add("后台调查", closeRunner)
			builtins = append(builtins, tools.ResearchTools(runner))
			kit.briefers = append(kit.briefers, runner)
			runner.OnFinished(func(task research.Task) { // 查完了：让他最近说话的那台终端响一声，叫醒后任务板会先说结论
				delivered := kit.notices.Notify(task.Person, realtimeproto.HaroNotice{Kind: realtimeproto.NoticeResearchDone})
				logger.Info("调查结束：通知终端", "id", task.ID, "status", task.Status, "terminals", delivered)
			})
		}
	}
	if len(builtins) == 0 && settings.MCPServersFile == "" {
		logger.Info("工具：内置的一组都没开，也没有外部的 MCP 服务")
		return kit, nil
	}
	box, err := tools.NewToolbox(ctx, logger, builtins...)
	if err != nil {
		return kit, err
	}
	shutdown.add("工具箱", func() error { box.Close(); return nil })
	kit.toolbox = box
	if settings.MCPServersFile != "" { // 外部的 MCP 服务：允许名单里的工具连上后加进清单（连不上的在后台接着连）
		servers, err := tools.LoadRemoteServers(settings.MCPServersFile, os.LookupEnv)
		if err != nil {
			return kit, fmt.Errorf("HARO_MCP_SERVERS_FILE：%w", err)
		}
		if err := box.AttachRemoteServers(ctx, servers, deps.httpClient); err != nil {
			return kit, err
		}
	}
	return kit, nil
}

// assembleBrain 装配大脑：代理（模型）、对话簿、主人资料、多终端仲裁、月度上限的口头提高。
func assembleBrain(ctx context.Context, settings config.ServerSettings, catalog agents.Catalog, deps services, kit tooling,
	logger *slog.Logger, shutdown *shutdownSteps) (brain.Components, brain.Options, brainExtras, error) {
	options, err := buildBrainOptions(settings)
	if err != nil {
		return brain.Components{}, options, brainExtras{}, err
	}
	built, err := newAgentAssembly(settings, catalog, deps, logger).build(kit.toolbox, kit.expertSearch)
	if err != nil {
		return brain.Components{}, options, brainExtras{}, err
	}
	logger.Info("预热模型（本地第一次可能要几十秒；云端不用预热）", "model", activeModel(settings))
	if err := brain.WarmUp(ctx, built.front.Model); err != nil {
		logger.Warn("模型预热失败，第一轮对话会比较慢", "err", err)
	}
	components := brain.Components{
		Streaming: deps.streaming, Front: built.front, TextFront: built.frontText, Handoff: built.handoff, FrameExpert: built.expert,
		Synthesizer: deps.synthesis, Tools: kit.toolbox, Notices: kit.notices,
		Briefers: append(kit.briefers, built.breaker), // 连不上之后刚恢复时，告诉模型断过多久
		NewSegmenter: func() brain.TextSegmenter {
			return speech.NewSentenceSegmenter(settings.FirstSegmentMinRunes, settings.MaxSegmentRunes)
		},
	}
	if built.overBudget != nil { // 月度上限到了以后，主人可以口头临时提高（判断交给代理「额度判断」）
		components.MonthlyCap = deps.guard
		components.OverBudget = built.overBudget
		logger.Info("月度上限到了以后可以口头临时提高", "raise", options.MonthlyRaise.String(), "daily_checks", settings.OverBudgetDailyCalls)
	}
	var store brain.ConversationStore // 没设对话库时是 nil 接口（不是装着 nil 指针的接口）
	if deps.conversations != nil {
		store = deps.conversations
	}
	components.Conversations = brain.NewConversationBook(store, options, logger) // 所有终端连接共用：换终端、重连、服务端重启都接得上
	if built.summary != nil {                                                    // 滚动摘要：代理「摘要」，按它自己的用途记账
		components.Conversations.SummarizeWith(*built.summary)
	}
	if options.IdleCompactAfter > 0 { // 闲了一阵的对话在后台提前压缩：随 ctx（服务端的运行期）结束
		go components.Conversations.RunIdleCompaction(ctx)
		logger.Info("对话闲了一阵就在后台提前压缩", "idle", options.IdleCompactAfter)
	}
	var extras brainExtras
	components.Profiles = kit.household                      // 主人资料：按说话的人所在的账号（管理员没在 App 里改过时用资料文件）
	if deps.conversations != nil && built.backstory != nil { // 背景记录池：从对话压出来，接在主人资料后面
		idle := options.IdleCompactAfter
		if idle <= 0 {
			idle = backstory.DefaultIdle
		}
		pool, err := backstory.Open(ctx, deps.conversations, backstory.AgentMerger{Agent: *built.backstory}, idle, logger)
		if err != nil {
			return brain.Components{}, options, brainExtras{}, err
		}
		go pool.Run(ctx)
		components.Profiles = backstory.WithProfile{Profiles: kit.household, Pool: pool}
		extras.backstory = pool
		logger.Info("背景记录池：对话闲下来后并入、每天整理，接在主人资料后面", "idle", idle, "max_runes", backstory.MaxRunes)
	}
	if settings.WakeArbitration > 0 { // 多台终端同时听到唤醒词：只让最合适的那台回答
		components.WakeArbiter = brain.NewWakeArbiter(settings.WakeArbitration)
	}
	extras.title = built.title
	return components, options, extras, nil
}

// brainExtras 是组装大脑时一起建好、App 的接口要用的：起标题的代理、背景记录池（没有时为 nil）。
type brainExtras struct {
	title     *brain.Agent
	backstory *backstory.Service
}

// openConversationStore 打开对话库（没设 HARO_CONVERSATIONS_DB 时为 nil：对话只在内存里）；打不开就不启动（不带着空历史开始）。
func openConversationStore(ctx context.Context, settings config.ServerSettings, shutdown *shutdownSteps) (*conversationstore.SQLiteStore, error) {
	if settings.ConversationsDB == "" {
		return nil, nil
	}
	store, err := conversationstore.OpenSQLite(ctx, settings.ConversationsDB)
	if err != nil {
		return nil, err
	}
	shutdown.add("对话库", store.Close)
	return store, nil
}

// serve 对外提供服务，直到收到退出信号：停止接新连接 → 结束会话（每一轮收尾、落盘）→ 返回后由 shutdownSteps 依次收尾。
func serve(ctx context.Context, settings config.ServerSettings, components brain.Components, options brain.Options, deps services,
	entry entrypoints, logger *slog.Logger) error {
	newSession := func(terminal brainserver.Terminal) brainserver.Session {
		parts := components
		parts.Residents = residentsOf(terminal) // 别的账号的终端只认它的本人
		if terminal.DeviceTools != nil {        // haro 终端：它的摄像头经 MCP 用（普通 Realtime 客户端没有）
			parts.Camera = terminaltools.ConnectCamera(terminal.DeviceTools, buildinfo.Version, logger)
		}
		return brain.NewSession(parts, options, logger)
	}
	listener, err := net.Listen("tcp", settings.ServerListen)
	if err != nil {
		return err
	}
	checks, err := readinessChecks(settings, deps)
	if err != nil {
		return err
	}
	monitor := readiness.NewMonitor(checks, readinessInterval, readinessTimeout, logger)
	go monitor.Run(ctx)
	handler, err := brainHandler(settings, newSession, monitor, entry, logger)
	if err != nil {
		return err
	}
	logger.Info("haro-server 启动", "version", buildinfo.Version, "listen", listener.Addr().String(), "model", activeModel(settings), "tts", settings.TTS,
		"personas", len(options.Personas))
	if _, err := graceful.Serve(ctx, listener, handler, graceful.Timing{Shutdown: shutdownRequests, Drain: shutdownSessions},
		logger); err != nil {
		return err
	}
	return ctx.Err()
}

// householdStore 是「家」那一层存设置的地方：App 的接口没开（没有账号库）时为 nil，只用服务端的配置。
func householdStore(opened appAccounts) household.Store {
	if opened.store == nil {
		return nil
	}
	return opened.store
}
