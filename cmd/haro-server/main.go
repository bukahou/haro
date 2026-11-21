// haro-server：haro 的服务端（架构见 docs/architecture.md）。
// 识别（haro-asr，单独运行）→ 模型 → 工具 → 性格 → 语音合成（GPT-SoVITS 服务或 Piper），
// 经 WebSocket（Realtime 事件）为终端 haro 服务。一条连接一个会话。
// 这里只做组装：读配置 → 创建各段实现 → 挂到 HTTP 服务上。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // 内置时区数据：镜像里没有 /usr/share/zoneinfo 时 TZ（例如 Asia/Tokyo）照样生效（集群里默认是 UTC）

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/graceful"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/persona"
	"github.com/bukahou/haro/internal/pricecatalog"
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

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.Describe("haro-server"))
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", "haro-server")
	slog.SetDefault(logger)
	if err := run(logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("haro-server 退出", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	settings, err := config.LoadBrainSettings()
	if err != nil {
		return err
	}
	for _, note := range settings.RemovedSettingNotes {
		logger.Warn(note)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.SetupTelemetry(ctx, settings.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() { // 最后一步：在期限内把还没发出的遥测发出去（采集器连不上时不卡住退出）
		flushCtx, cancel := context.WithTimeout(context.Background(), shutdownTelemetryFlush)
		defer cancel()
		_ = shutdownTelemetry(flushCtx)
	}()

	httpClient := &http.Client{Timeout: settings.RequestTimeout}
	// 模型的流式请求不用带总超时的客户端：客户端的总超时连读流也算在内，各用途要的长短不同 ⇒
	// 每个模型按用途给请求设期限（cloudModelSpec.requestTimeout；调查受它自己的总时限管）
	modelHTTP := &http.Client{}
	// Claude 的客户端全进程只建一个，所有用途共用（见 llm.ClaudeAPI：多一个客户端 / HTTP 客户端就多一份令牌缓存，
	// 集群令牌只能换一次 ⇒ 互相把令牌用掉）。不用 Claude 时为 nil
	claude := newClaudeAPI(settings, modelHTTP)
	if _, err := llm.ParseToolCallIDStyle(settings.CloudLLMToolCallIDs); err != nil {
		return fmt.Errorf("HARO_CLOUD_LLM_TOOL_CALL_IDS: %w", err)
	}

	synthesizer, err := buildSynthesizer(settings, httpClient, logger)
	if err != nil {
		return err
	}
	streaming := speech.NewRealtimeTranscriptionClient(settings.ASRURL)
	logger.Info("语音识别：haro-asr", "url", settings.ASRURL)

	// 用量账本与额度：先有刹车，再接搜索、后台调查
	ledger, err := spending.OpenLedger(ctx, settings.UsageDB)
	if err != nil {
		return err
	}
	ledger = spending.WithChargeLog(ledger, logger)
	defer ledger.Close()
	if settings.UsageDB == "" {
		logger.Warn("没设 HARO_USAGE_DB：用量只记在内存里，服务端重启后额度重新计算")
	}
	guard := spending.NewGuard(ledger, spending.Limits{ResearchDailyYen: settings.ResearchDailyYen,
		MonthlyYen: settings.MonthlyYen, SearchDailyCredits: settings.SearchDailyCredits, WarnRatio: 0.8,
		MonthlyRaiseYen: settings.MonthlyRaiseYen}, logger)
	logger.Info("费用上限", "research_daily_yen", settings.ResearchDailyYen, "monthly_yen", settings.MonthlyYen,
		"search_daily_credits", settings.SearchDailyCredits)
	pricing, err := loadPricing(ctx, settings, httpClient, logger)
	if err != nil {
		return err
	}
	briefers := []brain.Briefer{brain.ClockBriefer{}, guard} // 每轮补充给模型的即时情况：现在几号几点、额度提醒、任务板
	notices := brain.NewNoticeHub()                          // 给终端的提示（后台调查查完了让终端响一声）
	var toolbox brain.Toolbox
	var expertSearch vision.Search // 看画面的专家联网查：和主对话的快查同一套搜索与额度
	if settings.Tools {
		// 自研工具内置：记事本直接连数据库；MCP 只接第三方（现在还没有）
		store, err := notes.Open(ctx, settings.NotesDB)
		if err != nil {
			return err
		}
		defer store.Close()
		builtins := []tools.Builtin{tools.NoteTools(store, settings.NotesListLimit, logger)}
		if settings.Weather { // 天气：Open-Meteo 为主，日本气象厅备用；都不用密钥
			weatherHTTP := weather.NewHTTPClient() // 连接等得短：连不上就早点交给备用
			home := weatherHome(ctx, settings, weatherHTTP, logger)
			openMeteo := weather.NewClient(settings.WeatherForecastURL, settings.WeatherGeocodingURL, weatherHTTP)
			forecaster := weatherForecaster(ctx, settings, openMeteo, home, weatherHTTP, logger)
			builtins = append(builtins, tools.WeatherTools(openMeteo, forecaster, home, settings.WeatherGeocodingLanguage, logger))
			if home != nil {
				briefers = append(briefers, brain.BriefLines(weather.LocationNote{Home: home}.Brief)) // 每轮告诉模型主人家大致在哪（城市级）
			}
		}
		if searcher := connectWebSearch(settings, httpClient, logger); searcher != nil {
			defer searcher.Close()
			meter := spending.NewSearchMeter(guard, ledger, pricing, spending.PurposeSearch, "tavily")
			builtins = append(builtins, tools.WebSearchTools(searcher, meter, logger))
			expertSearch = func(ctx context.Context, query string) (string, error) {
				return tools.QuickSearch(ctx, searcher, meter, query, logger)
			}
			runner, closeRunner, err := startResearch(ctx, settings, claude, modelHTTP, pricing, searcher, ledger, guard, logger)
			if err != nil {
				return err
			}
			defer closeRunner()
			builtins = append(builtins, runner.Tools())
			briefers = append(briefers, runner)
			runner.OnFinished(func(task research.Task) { // 查完了：让他最近说话的那台终端响一声，叫醒后任务板会先说结论
				delivered := notices.Notify(task.Person, realtimeproto.HaroNotice{Kind: realtimeproto.NoticeResearchDone})
				logger.Info("调查结束：通知终端", "id", task.ID, "status", task.Status, "terminals", delivered)
			})
		}
		box, err := tools.NewToolbox(ctx, nil, logger, builtins...)
		if err != nil {
			return err
		}
		defer box.Close()
		toolbox = box
	}

	options, err := buildBrainOptions(settings)
	if err != nil {
		return err
	}
	model, breaker := buildChatModel(settings, claude, modelHTTP, pricing, ledger, guard, spending.PurposeChat, logger)
	briefers = append(briefers, breaker) // 连不上之后刚恢复时，告诉模型断过多久
	logger.Info("预热模型（本地第一次可能要几十秒；云端不用预热）", "model", activeModel(settings))
	if err := brain.WarmUp(ctx, model); err != nil {
		logger.Warn("模型预热失败，第一轮对话会比较慢", "err", err)
	}
	components := brain.Components{
		Streaming: streaming, Model: model, Synthesizer: synthesizer, Tools: toolbox,
		Briefers: briefers, Notices: notices,
		NewSegmenter: func() brain.TextSegmenter {
			return speech.NewSentenceSegmenter(settings.FirstSegmentMinRunes, settings.MaxSegmentRunes)
		},
	}
	if settings.VisionExpert() {
		components.FrameExpert = newVisionExpert(settings, claude, modelHTTP, pricing, ledger, guard, expertSearch, logger)
	}
	if settings.MonthlyYen > 0 && settings.MonthlyRaiseYen > 0 && settings.LLMProvider == config.LLMCloud {
		// 月度上限到了以后，主人可以口头临时提高：判断用的小调用不受额度管（否则上限到了就没法判断），按天限次数、照样记账
		components.MonthlyCap = guard
		components.OverBudgetModel = spending.NewDailyCallLimit(spending.NewMeteredChatModel(newCloudChatModel(settings, claude,
			cloudModelSpec{model: settings.CloudLLMModel, maxTokens: overBudgetMaxTokens, effort: settings.CloudLLMEffort,
				thinking: settings.CloudLLMThinking, firstTokenTimeout: settings.CloudLLMTimeout, requestTimeout: settings.RequestTimeout},
			modelHTTP), settings.CloudLLMModel, spending.PurposeChat, pricing, ledger, logger), settings.OverBudgetDailyCalls)
		logger.Info("月度上限到了以后可以口头临时提高", "raise_yen", settings.MonthlyRaiseYen, "daily_checks", settings.OverBudgetDailyCalls)
	}
	// 对话簿所有终端连接共用：换终端、重连、服务端重启都接得上
	var conversationStore brain.ConversationStore
	if settings.ConversationsDB != "" {
		store, err := conversationstore.OpenSQLite(ctx, settings.ConversationsDB)
		if err != nil {
			return err
		}
		defer store.Close()
		if settings.ConversationsDir != "" { // 旧的对话文件：导入一次（出错就不启动，不带着空历史开始）
			imported, err := conversationstore.ImportJSONDir(ctx, store, settings.ConversationsDir, logger)
			if err != nil {
				return fmt.Errorf("导入旧的对话文件: %w", err)
			}
			if imported > 0 {
				logger.Info("旧的对话文件已导入对话库", "people", imported, "dir", settings.ConversationsDir)
			}
		}
		conversationStore = store
	}
	components.Conversations = brain.NewConversationBook(conversationStore, options, logger)
	if settings.ProfileDir != "" { // 主人资料：外部注入，改了文件下一轮就生效
		profiles := profile.OpenDir(settings.ProfileDir, logger)
		household, people := profiles.Summary()
		logger.Info("主人资料", "dir", settings.ProfileDir, "household_runes", household, "people", people)
		components.Profiles = profiles
	}
	if settings.ConversationSummary { // 滚动摘要：和主对话同一套模型，按 summary 用途记账
		summarizer, _ := buildChatModel(settings, claude, modelHTTP, pricing, ledger, guard, spending.PurposeSummary,
			logger.With("model_use", "summary"))
		components.Conversations.SummarizeWith(summarizer)
	}
	if settings.WakeArbitration > 0 { // 多台终端同时听到唤醒词：只让最合适的那台回答
		components.WakeArbiter = brain.NewWakeArbiter(settings.WakeArbitration)
	}
	newSession := func(terminal brainserver.Terminal) brainserver.Session {
		parts := components
		if terminal.DeviceTools != nil { // haro 终端：它的摄像头经 MCP 用（普通 Realtime 客户端没有）
			parts.Camera = terminaltools.ConnectCamera(terminal.DeviceTools, buildinfo.Version, logger)
		}
		return brain.NewSession(parts, options, logger)
	}

	listener, err := net.Listen("tcp", settings.ServerListen)
	if err != nil {
		return err
	}
	checks, err := readinessChecks(settings, claude, ledger, httpClient)
	if err != nil {
		return err
	}
	monitor := readiness.NewMonitor(checks, readinessInterval, readinessTimeout, logger)
	go monitor.Run(ctx)
	handler, err := brainHandler(settings, newSession, monitor, logger)
	if err != nil {
		return err
	}
	logger.Info("haro-server 启动", "version", buildinfo.Version, "listen", listener.Addr().String(), "model", activeModel(settings), "tts", settings.TTS,
		"personas", len(options.Personas))
	// 收到退出信号：停止接新连接 → 结束会话（每一轮收尾、落盘）→ 返回后 defer 依次停后台调查、关数据库、发出遥测
	if _, err := graceful.Serve(ctx, listener, handler, graceful.Timing{Shutdown: shutdownRequests, Drain: shutdownSessions},
		logger); err != nil {
		return err
	}
	return ctx.Err()
}

// 关停各步的期限（加起来 10 秒，比 k8s 的宽限期 30 秒短）：停止接新连接、等普通请求 2 秒 → 会话收尾 5 秒 →
// 后台调查停下 2 秒 → 遥测发出 1 秒。
const (
	shutdownRequests       = 2 * time.Second
	shutdownSessions       = 5 * time.Second
	shutdownResearch       = 2 * time.Second
	shutdownTelemetryFlush = 1 * time.Second
)

// brainHandler 是服务端的全部 HTTP 入口：对话（WebSocket）+ 远程登记声纹（转给 haro-asr）+ 存活 / 就绪检查。
// 终端只认识服务端的地址；haro-asr 只监听本机（集群里在同一个 Pod）。
func brainHandler(settings config.Settings, newSession brainserver.SessionFactory, monitor *readiness.Monitor,
	logger *slog.Logger) (http.Handler, error) {
	asrURL, err := url.Parse(settings.ASRURL)
	if err != nil {
		return nil, fmt.Errorf("HARO_ASR_URL: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", brainserver.NewHandler(newSession, brainserver.Options{}, logger))
	mux.Handle(realtimeproto.VoiceprintRoute, httputil.NewSingleHostReverseProxy(asrURL))
	mux.HandleFunc("GET /livez", readiness.Alive)
	mux.Handle("GET /readyz", monitor)
	return mux, nil
}

// 就绪检查每 readinessInterval 一轮，每项最多等 readinessTimeout；k8s 的就绪探针只读结果。
const (
	readinessInterval = 30 * time.Second
	readinessTimeout  = 5 * time.Second
)

// readinessChecks 是服务端的就绪检查：识别、合成连得上，云端模型的凭证能用，账本写得进去。
func readinessChecks(settings config.Settings, claude *llm.ClaudeAPI, ledger spending.Ledger, httpClient *http.Client) ([]readiness.Check, error) {
	asrReady, err := url.JoinPath(settings.ASRURL, "readyz")
	if err != nil {
		return nil, fmt.Errorf("HARO_ASR_URL: %w", err)
	}
	checks := []readiness.Check{
		{Name: "asr", Probe: readiness.HTTPStatusOK(httpClient, asrReady)},
		{Name: "tts", Probe: readiness.HTTPReachable(httpClient, settings.TTSURL)},
		{Name: "ledger", Probe: ledger.CheckWritable},
	}
	if claude != nil && settings.UsesCloudLLM() {
		checks = append(checks, readiness.Check{Name: "llm", Probe: func(ctx context.Context) error {
			return claude.CheckCredentials(ctx, settings.CloudLLMModel)
		}})
	}
	return checks, nil
}

// buildChatModel 按 HARO_LLM_PROVIDER 组装服务端用的模型（cloud = 云端，local = 本地 ollama；不互相顶替）。
// 外面依次套：连不上时的熔断（交回的 breaker 也是每轮情况说明的来源）、按月度额度放行。
//
// purpose 是记账用途：主对话（chat）与滚动摘要（summary，R4）各建一套，费用分开记。
func buildChatModel(settings config.Settings, claude *llm.ClaudeAPI, client *http.Client, pricing spending.Pricing, ledger spending.Ledger, guard *spending.Guard,
	purpose spending.Purpose, logger *slog.Logger) (brain.ChatModel, *llm.UnavailabilityBreaker) {
	maxTokens := settings.CloudMaxTokens
	if purpose == spending.PurposeSummary && maxTokens < summaryMaxTokens {
		maxTokens = summaryMaxTokens // 摘要最长 300 字，聊天的输出上限可能不够
	}
	// quick：当主模型时快速失败 —— 等不到开始回答（CloudLLMTimeout）就取消、交给退路
	firstTokenTimeout := func(quick bool) time.Duration {
		if quick {
			return settings.CloudLLMTimeout
		}
		return 0
	}
	// 每个具体模型都记账（本地模型在价格表里标了不花钱，照样记 token）
	newLocal := func(quick bool) brain.ChatModel {
		return spending.NewMeteredChatModel(llm.NewOllamaChatModel(settings.OllamaURL, settings.Model, maxTokens, llm.OpenAIOptions{
			HTTPClient: client, FirstTokenTimeout: firstTokenTimeout(quick), RequestTimeout: settings.RequestTimeout}),
			settings.Model, purpose, pricing, ledger, logger)
	}
	newCloud := func(quick bool) brain.ChatModel {
		// 不在 SDK 里重试：失败时由退路接手，不让主人多等
		return spending.NewMeteredChatModel(newCloudChatModel(settings, claude, cloudModelSpec{model: settings.CloudLLMModel,
			maxTokens: maxTokens, effort: settings.CloudLLMEffort, thinking: settings.CloudLLMThinking,
			firstTokenTimeout: firstTokenTimeout(quick), requestTimeout: settings.RequestTimeout}, client),
			settings.CloudLLMModel, purpose, pricing, ledger, logger)
	}
	// 本地、云端二选一，不自动互相顶替（第 7 段）：连不上时只保留最低能力，额度用完时不回答
	var paid brain.ChatModel
	if settings.LLMProvider == config.LLMLocal {
		logger.Info("服务端：本地模型", "model", settings.Model)
		paid = newLocal(true)
	} else {
		logger.Info("服务端：云端模型", "model", settings.CloudLLMModel)
		paid = newCloud(true)
	}
	if purpose == spending.PurposeChat {
		paid = routeToStrong(settings, claude, client, paid, maxTokens, pricing, ledger, logger)
	}
	breaker := llm.NewUnavailabilityBreaker(paid, unavailableRetry, logger)
	return spending.NewBudgetedChatModel(breaker, guard, logger), breaker
}

// routeToStrong 按规则把看画面（takeover 时）、前台请它重答的请求交给更强的模型（HARO_STRONG_LLM_*），其余照旧；
// 更强的模型出错或等太久（HARO_STRONG_LLM_TIMEOUT）时由原来那套接手。没设更强的模型、或这两类都不用它时原样返回。
func routeToStrong(settings config.Settings, claude *llm.ClaudeAPI, client *http.Client, base brain.ChatModel, maxTokens int,
	pricing spending.Pricing, ledger spending.Ledger, logger *slog.Logger) brain.ChatModel {
	vision, correction := settings.VisionTakeover(), settings.StrongLLMFor(config.StrongForCorrection)
	if !vision && !correction {
		return base
	}
	strong := spending.NewMeteredChatModel(newCloudChatModel(settings, claude, cloudModelSpec{model: settings.StrongLLMModel,
		maxTokens: maxTokens, effort: settings.StrongLLMEffort, thinking: settings.StrongLLMThinking,
		firstTokenTimeout: settings.StrongLLMTimeout, requestTimeout: settings.RequestTimeout}, client),
		settings.StrongLLMModel, spending.PurposeChat, pricing, ledger, logger)
	strongOrBase := llm.NewFallbackChatModel(strong, "strong", base, "cloud", strongCooldown, logger)
	options := llm.RouterOptions{Default: base, Logger: logger}
	if vision {
		options.Vision, options.VisionName = strongOrBase, settings.StrongLLMModel
	}
	if correction {
		options.Correction, options.CorrectionName = strongOrBase, settings.StrongLLMModel
	}
	logger.Info("选模型：按规则交给更强的模型", "model", settings.StrongLLMModel, "use", settings.StrongLLMUse,
		"vision_mode", settings.StrongLLMVisionMode, "effort", settings.StrongLLMEffort, "thinking", settings.StrongLLMThinking)
	return llm.NewRouterChatModel(options)
}

// newVisionExpert 创建看画面的幕后专家（HARO_STRONG_LLM_VISION_MODE=expert）：用更强的模型，按 vision 用途记账、查月度额度。
// 它出错或等太久时，brain 退回把画面直接交给前台模型（不经选模型的包装层，前台照常是云端模型）。
func newVisionExpert(settings config.Settings, claude *llm.ClaudeAPI, client *http.Client, pricing spending.Pricing, ledger spending.Ledger, guard *spending.Guard,
	search vision.Search, logger *slog.Logger) brain.FrameExpert {
	model := spending.NewMeteredChatModel(newCloudChatModel(settings, claude, cloudModelSpec{model: settings.StrongLLMModel,
		maxTokens: settings.CloudMaxTokens, effort: settings.StrongLLMEffort, thinking: settings.StrongLLMThinking,
		firstTokenTimeout: settings.StrongLLMTimeout, requestTimeout: settings.RequestTimeout}, client),
		settings.StrongLLMModel, spending.PurposeVision, pricing, ledger, logger)
	logger.Info("看画面：交给幕后专家，判断由前台转述", "model", settings.StrongLLMModel, "search", search != nil)
	return vision.NewExpert(spending.NewBudgetedChatModel(model, guard, logger), settings.StrongLLMModel, search, logger)
}

// newClaudeAPI 创建全进程共用的 Claude 客户端（HARO_CLOUD_LLM_API=anthropic 时）；其余情况为 nil。
func newClaudeAPI(settings config.Settings, httpClient *http.Client) *llm.ClaudeAPI {
	if settings.CloudLLMAPI != config.CloudAPIAnthropic {
		return nil
	}
	return llm.NewClaudeAPI(settings.AnthropicAPIKey, settings.CloudLLMURL, httpClient)
}

// cloudModelSpec 是一个云端模型的参数：同一个云端接口，不同用途（聊天、更强的模型、看画面、调查）各一套。
type cloudModelSpec struct {
	model             string
	maxTokens         int
	effort, thinking  string        // 只对 anthropic 有效
	retries           int           // SDK 在 429 / 5xx / 断线时自己重试几次
	firstTokenTimeout time.Duration // 等开始回答最多多久；0 = 不限
	requestTimeout    time.Duration // 整次请求最多多久；0 = 只受调用方的 context 管
}

// newCloudChatModel 按 HARO_CLOUD_LLM_API 创建云端的 model：openai = OpenAI 兼容接口（用 client），
// anthropic = Claude API（用共用的 claude）。
func newCloudChatModel(settings config.Settings, claude *llm.ClaudeAPI, spec cloudModelSpec, client *http.Client) brain.ChatModel {
	if settings.CloudLLMAPI == config.CloudAPIAnthropic {
		return llm.NewAnthropicChatModel(llm.AnthropicOptions{API: claude, Model: spec.model, MaxTokens: spec.maxTokens,
			Effort: spec.effort, Thinking: spec.thinking, MaxRetries: spec.retries, FirstTokenTimeout: spec.firstTokenTimeout,
			RequestTimeout: spec.requestTimeout})
	}
	return llm.NewOpenAIChatModel(llm.OpenAIOptions{BaseURL: settings.CloudLLMURL, Model: spec.model, APIKey: settings.CloudLLMAPIKey,
		MaxTokens: spec.maxTokens, HTTPClient: client, ToolCallIDs: llm.ToolCallIDStyle(settings.CloudLLMToolCallIDs),
		MaxRetries: spec.retries, FirstTokenTimeout: spec.firstTokenTimeout, RequestTimeout: spec.requestTimeout})
}

// loadPricing 是把用量折成日元的单价表：云端模型（和设了的更强的模型）的单价从模型价格目录取（启动时拉取，拉不到用上次存的；
// 都没有、或目录里没有用到的模型就起不来 —— 费用上限要靠单价折算），本地模型标成不花钱；汇率、搜索积分的价格来自配置。
// 不用云端模型时不拉目录。
func loadPricing(ctx context.Context, settings config.Settings, client *http.Client, logger *slog.Logger) (spending.Pricing, error) {
	pricing := spending.Pricing{USDToYen: settings.USDToYen, SearchCreditUSD: settings.SearchCreditUSD,
		Models: map[string]spending.TokenPrice{settings.Model: {Free: true}}}
	var cloudModels []string
	if settings.UsesCloudLLM() {
		cloudModels = append(cloudModels, settings.CloudLLMModel)
	}
	if settings.StrongLLMModel != "" {
		cloudModels = append(cloudModels, settings.StrongLLMModel)
	}
	if len(cloudModels) == 0 {
		return pricing, nil
	}
	catalog, report, err := pricecatalog.Load(ctx, client, settings.ModelPricesURL, settings.ModelPricesProvider, settings.ModelPricesCache)
	if err != nil {
		return pricing, fmt.Errorf("模型单价: %w", err)
	}
	switch {
	case report.FromCache:
		logger.Warn("模型价格目录拉不到：用上次存的那份", "err", report.FetchErr, "fetched_at", catalog.FetchedAt,
			"cache", settings.ModelPricesCache)
	case report.SaveErr != nil:
		logger.Warn("模型价格目录拉到了，但没存到本地（下次拉不到时就没有可退的）", "err", report.SaveErr,
			"cache", settings.ModelPricesCache)
	case settings.ModelPricesCache == "":
		logger.Warn("没设 HARO_MODEL_PRICES_CACHE：模型价格目录拉到的不存，下次拉不到时服务端起不来")
	}
	for _, model := range cloudModels {
		price, err := catalog.Price(model)
		if err != nil {
			return pricing, fmt.Errorf("模型单价（%s）: %w", settings.ModelPricesURL, err)
		}
		pricing.Models[model] = price
		logger.Info("模型单价（每百万 token 的美元价，来自模型价格目录）", "model", model, "provider", catalog.Provider,
			"input", price.InputUSDPerMillion, "output", price.OutputUSDPerMillion, "cache_read", price.CacheReadUSDPerMillion,
			"cache_write", price.CacheWriteUSDPerMillion, "tiers", len(price.Tiers), "fetched_at", catalog.FetchedAt)
	}
	return pricing, nil
}

// weatherHome 是家的位置（已在加载配置时校验过）：配置的坐标，或按公网 IP 定位；没设时为 nil。
// IP 定位在启动时先做一次并写进日志：精度是城市级，偏了就改填坐标。
func weatherHome(ctx context.Context, settings config.Settings, httpClient *http.Client, logger *slog.Logger) weather.HomeLocator {
	home, _ := settings.WeatherHomePlace()
	switch {
	case !home.Set:
		logger.Info("天气：没设家的位置（HARO_WEATHER_HOME），问天气时要说地名")
		return nil
	case home.ByIP:
		locator := weather.NewIPHome(settings.WeatherIPLocateURL, settings.WeatherHomeName, httpClient)
		if place, err := locator.Home(ctx); err != nil {
			logger.Warn("天气：按 IP 定位家的位置失败（问天气时再试）", "err", err)
		} else {
			logger.Info("天气：按 IP 推定家在这附近（城市级；偏了就在 HARO_WEATHER_HOME 填坐标）",
				"place", place.Name, "region", place.Region, "country", place.Country)
		}
		return locator
	default:
		return weather.FixedHome{Place: weather.Place{Name: settings.WeatherHomeName, Latitude: home.Latitude, Longitude: home.Longitude}}
	}
}

// weatherForecaster 是查天气用的来源：Open-Meteo 为主；HARO_WEATHER_BACKUP=jma 时查不到改用日本气象厅（只覆盖日本）。
// 家所在的预报地区在后台先找好并写进日志：地区是按坐标自动找的，偏了能从日志看出来；第一次用备用时也不用再下载地区表。
func weatherForecaster(ctx context.Context, settings config.Settings, primary *weather.Client, home weather.HomeLocator,
	httpClient *http.Client, logger *slog.Logger) weather.Forecaster {
	if settings.WeatherBackup != config.WeatherBackupJMA {
		return primary
	}
	jma := weather.NewJMA(settings.WeatherJMAURL, settings.WeatherReverseGeocoderURL, httpClient)
	if home != nil {
		go func() {
			locateCtx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			place, err := home.Home(locateCtx)
			if err == nil {
				var area weather.JMAArea
				if area, err = jma.Locate(locateCtx, place); err == nil {
					logger.Info("天气备用（日本气象厅）：家所在的预报地区", "area", area.Describe(),
						"office", area.Office, "class10", area.Class10, "stations", area.Stations)
					return
				}
			}
			if errors.Is(err, weather.ErrNotCovered) {
				logger.Info("天气备用（日本气象厅）：家不在日本，用不上")
				return
			}
			logger.Warn("天气备用（日本气象厅）：没能先找好家所在的预报地区（用到时再找）", "err", err)
		}()
	}
	return weather.WithBackup(primary, "Open-Meteo", jma, "日本气象厅", logger)
}

// connectWebSearch 准备联网搜索（这时不连：用到时才连，断了自动重连）；没设密钥时返回 nil。
func connectWebSearch(settings config.Settings, httpClient *http.Client, logger *slog.Logger) *tools.TavilySearcher {
	if settings.TavilyAPIKey == "" {
		logger.Info("联网搜索：没设 HARO_TAVILY_API_KEY，不提供")
		return nil
	}
	logger.Info("联网搜索：Tavily（用到时才连，断了自动重连）")
	return tools.NewTavilySearcher(settings.TavilyMCPURL, settings.TavilyAPIKey, httpClient, logger)
}

// startResearch 开启后台调查：任务账本、调查用的模型、上限，并在后台开始处理队列。
func startResearch(ctx context.Context, settings config.Settings, claude *llm.ClaudeAPI, client *http.Client, pricing spending.Pricing, searcher tools.WebSearcher,
	ledger spending.Ledger, guard *spending.Guard, logger *slog.Logger) (*research.Runner, func(), error) {
	store, err := research.OpenTaskStore(ctx, settings.ResearchDB)
	if err != nil {
		return nil, nil, err
	}
	if settings.ResearchDB == "" {
		logger.Warn("没设 HARO_RESEARCH_DB：调查任务只记在内存里，服务端重启后就忘了")
	}
	// 调查在后台、不急：限流 / 服务端一时出错时让 SDK 重试两次；每次请求不另设期限，受调查自己的总时限管
	model := spending.NewMeteredChatModel(newCloudChatModel(settings, claude, cloudModelSpec{model: settings.CloudLLMModel,
		maxTokens: researchMaxTokens, effort: settings.ResearchEffort, thinking: settings.CloudLLMThinking, retries: 2}, client),
		settings.CloudLLMModel, spending.PurposeResearch, pricing, ledger, logger)
	limits := research.DefaultLimits
	limits.Timeout, limits.MaxRounds = settings.ResearchTimeout, settings.ResearchMaxRounds
	limits.MaxSearches, limits.MaxTokens = settings.ResearchMaxSearches, settings.ResearchMaxTokens
	meter := spending.NewSearchMeter(guard, ledger, pricing, spending.PurposeResearch, "tavily")
	runner := research.NewRunner(store, model, searcher, meter, guard, limits, logger)
	writer := "（调查模型自己写）"
	if settings.StrongLLMFor(config.StrongForResearch) { // 搜索和读网页用便宜的模型，结论交给更强的模型写
		runner.WriteConclusionsWith(spending.NewMeteredChatModel(newCloudChatModel(settings, claude, cloudModelSpec{model: settings.StrongLLMModel,
			maxTokens: researchMaxTokens, effort: settings.ResearchEffort, thinking: settings.StrongLLMThinking, retries: 2}, client),
			settings.StrongLLMModel, spending.PurposeResearch, pricing, ledger, logger))
		writer = settings.StrongLLMModel
	}
	running := make(chan struct{})
	go func() {
		defer close(running)
		runner.Run(ctx) // ctx 随退出信号结束：手上的调查停下（下次启动标成被中断）
	}()
	logger.Info("后台调查已开启", "model", settings.CloudLLMModel, "conclusion_writer", writer, "timeout", limits.Timeout,
		"max_rounds", limits.MaxRounds, "max_searches", limits.MaxSearches, "max_tokens", limits.MaxTokens)
	stop := func() { // 先等调查的循环退出，再关任务账本：不然它最后一次写任务时库已经关了
		select {
		case <-running:
		case <-time.After(shutdownResearch):
			logger.Warn("后台调查没在期限内停下：照样关任务账本", "timeout", shutdownResearch)
		}
		store.Close()
	}
	return runner, stop, nil
}

// overBudgetMaxTokens：月度上限到了以后，判断主人要不要提高上限的那次小调用的输出上限（只交工具调用，不说话）。
const overBudgetMaxTokens = 200

// summaryMaxTokens 是写滚动摘要时每次请求的输出上限。
const summaryMaxTokens = 600

// researchMaxTokens 是调查时每次请求的输出上限（结论比聊天长，聊天用 HARO_CLOUD_LLM_MAX_TOKENS）。
const researchMaxTokens = 1000

// buildSynthesizer 按 HARO_TTS 选择语音合成实现（服务都单独运行，只按 HARO_TTS_URL 连）。
func buildSynthesizer(settings config.Settings, httpClient *http.Client, logger *slog.Logger) (brain.SpeechSynthesizer, error) {
	if settings.TTS == config.TTSGPTSoVITS {
		voice, err := speech.LoadGPTSoVITSVoice(settings.TTSVoiceFile)
		if err != nil {
			return nil, err
		}
		logger.Info("语音合成：GPT-SoVITS", "url", settings.TTSURL, "voice", voice.Name,
			"emotions", len(voice.References), "streaming_mode", settings.TTSStreamingMode)
		return speech.NewGPTSoVITSSynthesizer(settings.TTSURL, settings.TTSRefDir, voice, settings.TTSStreamingMode,
			settings.TTSTimeout, httpClient), nil
	}
	logger.Info("语音合成：Piper", "url", settings.TTSURL)
	return speech.NewPiperHTTPSynthesizer(settings.TTSURL, settings.TTSTimeout, httpClient), nil
}

// buildBrainOptions 读取性格档案；没设 HARO_PERSONAS_DIR 时不分性格。
func buildBrainOptions(settings config.Settings) (brain.Options, error) {
	phrases, err := brain.LoadPhrases(settings.PhrasesFile) // 不经模型、直接念的固定的话；没配就用程序里带的
	if err != nil {
		return brain.Options{}, err
	}
	// 基础提示词：内置的永远生效，HARO_SYSTEM_PROMPT 只能追加
	options := brain.Options{MaxHistoryTurn: settings.MaxHistoryTurn, Instructions: brain.ComposeInstructions(settings.ExtraSystemPrompt),
		ConfirmationTimeout: settings.ConfirmationTimeout, Phrases: phrases, MonthlyRaiseYen: settings.MonthlyRaiseYen,
		Escalation: settings.StrongLLMFor(config.StrongForCorrection)} // 和 routeToStrong 配「重答」那条路的条件一致
	if settings.PersonasDir == "" {
		return options, nil
	}
	personas, err := persona.LoadCatalog(settings.PersonasDir)
	if err != nil {
		return options, err
	}
	options.Personas, options.DefaultPersona = personas, settings.PersonaDefault
	if settings.PersonaState != "" {
		options.PersonaState = persona.NewFileState(settings.PersonaState)
	}
	return options, nil
}

// activeModel 返回实际在用的模型名（日志用）。
func activeModel(settings config.Settings) string {
	if settings.LLMProvider == config.LLMLocal {
		return settings.Model
	}
	return settings.CloudLLMModel
}

// strongCooldown：更强的模型出错或等太久之后，这么久内直接用原来那套（沿用原来云端↔本地退路的冷却时间）。
const strongCooldown = 30 * time.Second

// unavailableRetry：连不上模型服务之后，这么久内不再去连（马上告诉主人连不上），之后放一个请求去试（沿用同一个冷却时间）。
const unavailableRetry = 30 * time.Second
