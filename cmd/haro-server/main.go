// haro-server：haro 的服务端（架构见 docs/architecture.md）。
// 识别（haro-asr 子进程）→ 模型 → 工具 → 性格 → 语音合成（GPT-SoVITS 服务或 Piper），
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
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // 内置时区数据：镜像里没有 /usr/share/zoneinfo 时 TZ（例如 Asia/Tokyo）照样生效（集群里默认是 UTC）

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/netdiag"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/persona"
	"github.com/bukahou/haro/internal/profile"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/speech"
	"github.com/bukahou/haro/internal/spending"
	"github.com/bukahou/haro/internal/telemetry"
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
	defer shutdownTelemetry(context.Background())

	httpClient := &http.Client{Timeout: settings.RequestTimeout}
	// Claude 的客户端全进程只建一个，所有用途共用（见 llm.ClaudeAPI：多一个客户端 / HTTP 客户端就多一份令牌缓存，
	// 集群令牌只能换一次 ⇒ 互相把令牌用掉）。不用 Claude 时为 nil
	claude := newClaudeAPI(settings, httpClient)

	synthesizer, stopSynthesizer, err := buildSynthesizer(ctx, settings, httpClient, logger)
	if err != nil {
		return err
	}
	defer stopSynthesizer()

	streaming, stopRecognizer, err := buildRecognizer(ctx, settings, logger)
	if err != nil {
		return err
	}
	defer stopRecognizer()

	// 用量账本与额度：先有刹车，再接搜索、后台调查
	ledger, err := spending.OpenLedger(ctx, settings.UsageDB)
	if err != nil {
		return err
	}
	defer ledger.Close()
	if settings.UsageDB == "" {
		logger.Warn("没设 HARO_USAGE_DB：用量只记在内存里，服务端重启后额度重新计算")
	}
	guard := spending.NewGuard(ledger, spending.Limits{ResearchDailyYen: settings.ResearchDailyYen,
		LLMMonthlyYen: settings.MonthlyYen, SearchDailyCredits: settings.SearchDailyCredits, WarnRatio: 0.8}, logger)
	logger.Info("费用上限", "research_daily_yen", settings.ResearchDailyYen, "monthly_yen", settings.MonthlyYen,
		"search_daily_credits", settings.SearchDailyCredits)
	briefers := []brain.Briefer{brain.ClockBriefer{}, guard} // 每轮补充给模型的即时情况：现在几号几点、额度提醒、任务板
	notices := brain.NewNoticeHub()                          // 给终端的提示（后台调查查完了让终端响一声）
	var toolbox brain.Toolbox
	var expertSearch vision.Search // 看画面的专家联网查：和主对话的快查同一套搜索与额度
	if settings.Tools {
		// 自研工具内置：记事本直接连数据库，网络诊断在本进程里执行；MCP 只接第三方（现在还没有）
		store, err := notes.Open(ctx, settings.NotesDB)
		if err != nil {
			return err
		}
		defer store.Close()
		checker := netdiag.NewChecker(settings.NetdiagDNSName, settings.NetdiagInternetTarget)
		builtins := append(tools.NoteTools(store, logger), tools.NetworkDiagnostics(checker)...)
		if settings.Weather { // 天气：Open-Meteo 为主，日本气象厅备用；都不用密钥
			weatherHTTP := weather.NewHTTPClient() // 连接等得短：连不上就早点交给备用
			home := weatherHome(ctx, settings, weatherHTTP, logger)
			openMeteo := weather.NewClient(settings.WeatherForecastURL, settings.WeatherGeocodingURL, weatherHTTP)
			forecaster := weatherForecaster(ctx, settings, openMeteo, home, weatherHTTP, logger)
			builtins = append(builtins, tools.WeatherTools(openMeteo, forecaster, home, logger)...)
			if home != nil {
				briefers = append(briefers, weather.LocationNote{Home: home}) // 每轮告诉模型主人家大致在哪（城市级）
			}
		}
		if searcher := connectWebSearch(ctx, settings, httpClient, logger); searcher != nil {
			defer searcher.Close()
			meter := spending.NewSearchMeter(guard, ledger, pricingOf(settings), spending.PurposeSearch, "tavily")
			builtins = append(builtins, tools.WebSearchTools(searcher, meter, logger)...)
			expertSearch = func(ctx context.Context, query string) (string, error) {
				return tools.QuickSearch(ctx, searcher, meter, query, logger)
			}
			runner, closeRunner, err := startResearch(ctx, settings, claude, httpClient, searcher, ledger, guard, logger)
			if err != nil {
				return err
			}
			defer closeRunner()
			builtins = append(builtins, runner.Tools()...)
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
	model := buildChatModel(settings, claude, httpClient, ledger, guard, spending.PurposeChat, logger)
	logger.Info("预热模型（本地第一次可能要几十秒；云端不用预热）", "model", activeModel(settings))
	if err := model.Warmup(ctx); err != nil {
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
		components.FrameExpert = newVisionExpert(settings, claude, httpClient, ledger, guard, expertSearch, logger)
	}
	// 对话簿所有终端连接共用：换终端、重连、服务端重启都接得上
	var conversationStore brain.ConversationStore
	if settings.ConversationsDir != "" {
		conversationStore = conversationstore.NewFileStore(settings.ConversationsDir)
	}
	components.Conversations = brain.NewConversationBook(conversationStore, options, logger)
	if settings.ProfileDir != "" { // 主人资料：外部注入，改了文件下一轮就生效
		profiles := profile.OpenDir(settings.ProfileDir, logger)
		household, people := profiles.Summary()
		logger.Info("主人资料", "dir", settings.ProfileDir, "household_runes", household, "people", people)
		components.Profiles = profiles
	}
	if settings.ConversationSummary { // 滚动摘要：和主对话同一套模型，按 summary 用途记账
		components.Conversations.SummarizeWith(buildChatModel(settings, claude, httpClient, ledger, guard, spending.PurposeSummary,
			logger.With("model_use", "summary")))
	}
	if settings.WakeArbitration > 0 { // 多台终端同时听到唤醒词：只让最合适的那台回答
		components.WakeArbiter = brain.NewWakeArbiter(settings.WakeArbitration)
	}
	newSession := func() brainserver.Session { return brain.NewSession(components, options, logger) }

	listener, err := net.Listen("tcp", settings.ServerListen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: brainHandler(settings, newSession, logger), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("haro-server 启动", "version", buildinfo.Version, "listen", listener.Addr().String(), "model", activeModel(settings), "tts", settings.TTS,
		"personas", len(options.Personas))
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return ctx.Err()
}

// buildRecognizer 按 HARO_ASR 选择流式识别：local 把 haro-asr 作为子进程拉起；openai 连 HARO_ASR_URL 上的转写服务。
// 返回流式识别和清理函数。
func buildRecognizer(ctx context.Context, settings config.Settings, logger *slog.Logger) (brain.StreamingRecognizer, func(), error) {
	if settings.ASR == config.ASROpenAI {
		return speech.NewRealtimeTranscriptionClient(settings.ASRURL), func() {}, nil
	}
	args := []string{"-listen", "127.0.0.1:" + strconv.Itoa(settings.ASRPort),
		"-model-dir", settings.ASRModelDir, "-threads", strconv.Itoa(settings.ASRThreads)}
	if settings.VADModel != "" {
		args = append(args, "-vad-model", settings.VADModel)
	}
	if settings.TurnModel != "" {
		args = append(args, "-turn-model", settings.TurnModel)
	}
	if settings.KWSModelDir != "" && settings.WakeWordsFile != "" {
		args = append(args, "-kws-model-dir", settings.KWSModelDir, "-keywords-file", settings.WakeWordsFile,
			"-kws-threshold", strconv.FormatFloat(settings.KWSThreshold, 'f', -1, 64),
			"-kws-score", strconv.FormatFloat(settings.KWSScore, 'f', -1, 64))
	}
	if settings.SpeakerModel != "" { // 声纹
		args = append(args, "-speaker-model", settings.SpeakerModel, "-voiceprints-dir", settings.VoiceprintsDir,
			"-speaker-threshold", strconv.FormatFloat(settings.SpeakerThreshold, 'f', -1, 64),
			"-wake-speaker-threshold", strconv.FormatFloat(settings.WakeSpeakerThreshold, 'f', -1, 64))
	}
	asr, err := speech.StartChildServer(ctx, speech.ChildServerSpec{
		Name: "haro-asr", Command: settings.ASRBinary, Args: args,
		BaseURL: localURL(settings.ASRPort), ReadyPath: "/health", ReadyTimeout: 30 * time.Second,
		LogOutput: os.Stderr, // 说完判断的概率、识别结果都在 haro-asr 的日志里
	})
	if err != nil {
		return nil, nil, err
	}
	logger.Info("haro-asr 识别服务已就绪", "url", asr.BaseURL, "threads", settings.ASRThreads, "vad", settings.VADModel != "")
	return speech.NewRealtimeTranscriptionClient(asr.BaseURL), asr.Stop, nil
}

func localURL(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// brainHandler 是服务端对终端的全部 HTTP 入口：对话（WebSocket）+ 远程登记声纹（转给 haro-asr）。
// 终端只认识服务端的地址；haro-asr 只监听本机（容器部署时在同一个容器里）。
func brainHandler(settings config.Settings, newSession brainserver.SessionFactory, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", brainserver.NewHandler(newSession, logger))
	if settings.ASR == config.ASRLocal {
		asrURL, _ := url.Parse(localURL(settings.ASRPort))
		mux.Handle("POST /v1/voiceprints/{name}", httputil.NewSingleHostReverseProxy(asrURL))
	}
	return mux
}

// buildChatModel 按 HARO_LLM_PROVIDER 组装服务端用的模型，HARO_LLM_FALLBACK 时另一个当退路：
//   - cloud：云端为主、本地 ollama 退路
//   - local：本地 ollama 为主、云端退路（本地模型那台机器离线时自动改用云端）
//
// purpose 是记账用途：主对话（chat）与滚动摘要（summary，R4）各建一套，费用分开记。
func buildChatModel(settings config.Settings, claude *llm.ClaudeAPI, httpClient *http.Client, ledger spending.Ledger, guard *spending.Guard,
	purpose spending.Purpose, logger *slog.Logger) brain.ChatModel {
	// 当主模型时用快速失败的客户端：等响应头最多 CloudLLMTimeout，连不上 / 卡住时尽快换退路
	quickClient := &http.Client{Timeout: settings.RequestTimeout,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: settings.CloudLLMTimeout,
			DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}}
	pricing := pricingOf(settings)
	maxTokens := settings.CloudMaxTokens
	if purpose == spending.PurposeSummary && maxTokens < summaryMaxTokens {
		maxTokens = summaryMaxTokens // 摘要最长 300 字，聊天的输出上限可能不够
	}
	// 每个具体模型都记账（本地模型不在价目表里，按 0 日元记 token）
	newLocal := func(client *http.Client) brain.ChatModel {
		return spending.NewMeteredChatModel(
			llm.NewOllamaChatModel(settings.OllamaURL, settings.Model, settings.ContextSize, settings.ModelKeepAlive, client),
			settings.Model, purpose, pricing, ledger, logger)
	}
	// quick：当主模型时快速失败（等不到开始回答就交给退路）。OpenAI 兼容接口靠 quickClient 的响应头超时；
	// Claude 共用一个 HTTP 客户端，改用 FirstTokenTimeout（见 llm.ClaudeAPI）
	newCloud := func(client *http.Client, quick bool) brain.ChatModel {
		var firstTokenTimeout time.Duration
		if quick {
			firstTokenTimeout = settings.CloudLLMTimeout
		}
		// 不在 SDK 里重试：失败时由退路接手，不让主人多等
		return spending.NewMeteredChatModel(newCloudChatModel(settings, claude, settings.CloudLLMModel, maxTokens, settings.CloudLLMEffort,
			settings.CloudLLMThinking, 0, firstTokenTimeout, client), settings.CloudLLMModel, purpose, pricing, ledger, logger)
	}
	var paid, free brain.ChatModel // paid：平时用的那套（可能花钱）；free：月度额度用完后退回的本地模型
	switch {
	case settings.LLMProvider != config.LLMCloud && !settings.LLMFallback:
		logger.Info("服务端：本地模型", "model", settings.Model)
		paid = newLocal(httpClient)
		free = paid
	case settings.LLMProvider != config.LLMCloud:
		logger.Info("服务端：本地模型，失败退回云端", "model", settings.Model, "fallback", settings.CloudLLMModel)
		paid = llm.NewFallbackChatModel(newLocal(quickClient), "local", newCloud(httpClient, false), "cloud", settings.LLMCooldown, logger)
		free = newLocal(httpClient)
	case !settings.LLMFallback:
		logger.Info("服务端：云端模型（月度额度用完后不回答：没有本地退路）", "model", settings.CloudLLMModel)
		paid = newCloud(quickClient, true)
	default:
		logger.Info("服务端：云端模型，失败退回本地", "model", settings.CloudLLMModel, "fallback", settings.Model)
		paid = llm.NewFallbackChatModel(newCloud(quickClient, true), "cloud", newLocal(httpClient), "local", settings.LLMCooldown, logger)
		free = newLocal(httpClient)
	}
	if purpose == spending.PurposeChat {
		paid = routeToStrong(settings, claude, httpClient, paid, maxTokens, pricing, ledger, logger)
	}
	return spending.NewBudgetedChatModel(paid, free, guard, logger)
}

// routeToStrong 按规则把看画面（takeover 时）、前台请它重答的请求交给更强的模型（HARO_STRONG_LLM_*），其余照旧；
// 更强的模型出错或等太久（HARO_STRONG_LLM_TIMEOUT）时由原来那套接手。没设更强的模型、或这两类都不用它时原样返回。
func routeToStrong(settings config.Settings, claude *llm.ClaudeAPI, client *http.Client, base brain.ChatModel, maxTokens int,
	pricing spending.Pricing, ledger spending.Ledger, logger *slog.Logger) brain.ChatModel {
	vision, correction := settings.VisionTakeover(), settings.StrongLLMFor(config.StrongForCorrection)
	if !vision && !correction {
		return base
	}
	strong := spending.NewMeteredChatModel(newCloudChatModel(settings, claude, settings.StrongLLMModel, maxTokens, settings.StrongLLMEffort,
		settings.StrongLLMThinking, 0, settings.StrongLLMTimeout, client), settings.StrongLLMModel, spending.PurposeChat, pricing, ledger, logger)
	strongOrBase := llm.NewFallbackChatModel(strong, "strong", base, "cloud", settings.LLMCooldown, logger)
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
func newVisionExpert(settings config.Settings, claude *llm.ClaudeAPI, client *http.Client, ledger spending.Ledger, guard *spending.Guard,
	search vision.Search, logger *slog.Logger) brain.FrameExpert {
	model := spending.NewMeteredChatModel(newCloudChatModel(settings, claude, settings.StrongLLMModel, settings.CloudMaxTokens,
		settings.StrongLLMEffort, settings.StrongLLMThinking, 0, settings.StrongLLMTimeout, client),
		settings.StrongLLMModel, spending.PurposeVision, pricingOf(settings), ledger, logger)
	logger.Info("看画面：交给幕后专家，判断由前台转述", "model", settings.StrongLLMModel, "search", search != nil)
	return vision.NewExpert(spending.NewBudgetedChatModel(model, nil, guard, logger), settings.StrongLLMModel, search, logger)
}

// newClaudeAPI 创建全进程共用的 Claude 客户端（HARO_CLOUD_LLM_API=anthropic 时）；其余情况为 nil。
func newClaudeAPI(settings config.Settings, httpClient *http.Client) *llm.ClaudeAPI {
	if settings.CloudLLMAPI != config.CloudAPIAnthropic {
		return nil
	}
	return llm.NewClaudeAPI(settings.AnthropicAPIKey, settings.CloudLLMURL, httpClient)
}

// newCloudChatModel 按 HARO_CLOUD_LLM_API 创建云端的 model：openai = OpenAI 兼容接口（用 client），
// anthropic = Claude API（用共用的 claude；effort、thinking、retries、firstTokenTimeout 只对它有效）。
func newCloudChatModel(settings config.Settings, claude *llm.ClaudeAPI, model string, maxTokens int, effort, thinking string, retries int,
	firstTokenTimeout time.Duration, client *http.Client) brain.ChatModel {
	if settings.CloudLLMAPI == config.CloudAPIAnthropic {
		return llm.NewAnthropicChatModel(llm.AnthropicOptions{API: claude, Model: model, MaxTokens: maxTokens,
			Effort: effort, Thinking: thinking, MaxRetries: retries, FirstTokenTimeout: firstTokenTimeout})
	}
	return llm.NewOpenAIChatModel(settings.CloudLLMURL, model, settings.CloudLLMAPIKey, maxTokens, client)
}

// pricingOf 是把用量折成日元的单价（都来自配置）：云端模型，以及设了的话更强的模型。
func pricingOf(settings config.Settings) spending.Pricing {
	models := map[string]spending.TokenPrice{settings.CloudLLMModel: {
		InputUSDPerMillion: settings.CloudLLMPriceIn, OutputUSDPerMillion: settings.CloudLLMPriceOut,
		CacheReadUSDPerMillion: settings.CloudLLMPriceCacheRead, CacheWriteUSDPerMillion: settings.CloudLLMPriceCacheWrite}}
	if settings.StrongLLMModel != "" {
		models[settings.StrongLLMModel] = spending.TokenPrice{
			InputUSDPerMillion: settings.StrongLLMPriceIn, OutputUSDPerMillion: settings.StrongLLMPriceOut,
			CacheReadUSDPerMillion: settings.StrongLLMPriceCacheRead, CacheWriteUSDPerMillion: settings.StrongLLMPriceCacheWrite}
	}
	return spending.Pricing{USDToYen: settings.USDToYen, SearchCreditUSD: settings.SearchCreditUSD, Models: models}
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
		name := settings.WeatherHomeName
		if name == "" {
			name = "家"
		}
		return weather.FixedHome{Place: weather.Place{Name: name, Latitude: home.Latitude, Longitude: home.Longitude}}
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

// connectWebSearch 连接联网搜索；没设密钥或连不上时返回 nil：第三方服务出问题不影响服务端启动，只是没有联网搜索。
func connectWebSearch(ctx context.Context, settings config.Settings, httpClient *http.Client, logger *slog.Logger) *tools.TavilySearcher {
	if settings.TavilyAPIKey == "" {
		logger.Info("联网搜索：没设 HARO_TAVILY_API_KEY，不提供")
		return nil
	}
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	searcher, err := tools.ConnectTavily(connectCtx, settings.TavilyMCPURL, settings.TavilyAPIKey, httpClient, logger)
	if err != nil {
		logger.Warn("联网搜索：连不上 Tavily，这次不提供（重启服务端再试）", "err", err)
		return nil
	}
	return searcher
}

// startResearch 开启后台调查：任务账本、调查用的模型、上限，并在后台开始处理队列。
func startResearch(ctx context.Context, settings config.Settings, claude *llm.ClaudeAPI, httpClient *http.Client, searcher tools.WebSearcher,
	ledger spending.Ledger, guard *spending.Guard, logger *slog.Logger) (*research.Runner, func(), error) {
	store, err := research.OpenTaskStore(ctx, settings.ResearchDB)
	if err != nil {
		return nil, nil, err
	}
	if settings.ResearchDB == "" {
		logger.Warn("没设 HARO_RESEARCH_DB：调查任务只记在内存里，服务端重启后就忘了")
	}
	pricing := pricingOf(settings)
	var model brain.ChatModel
	if settings.ResearchLLM == config.LLMCloud {
		// 调查在后台、不急：限流 / 服务端一时出错时让 SDK 重试两次
		model = spending.NewMeteredChatModel(newCloudChatModel(settings, claude, settings.CloudLLMModel, researchMaxTokens, settings.ResearchEffort,
			settings.CloudLLMThinking, 2, 0, httpClient), settings.CloudLLMModel, spending.PurposeResearch, pricing, ledger, logger)
	} else {
		model = spending.NewMeteredChatModel(llm.NewOllamaChatModel(settings.OllamaURL, settings.Model, settings.ContextSize,
			settings.ModelKeepAlive, httpClient), settings.Model, spending.PurposeResearch, pricing, ledger, logger)
	}
	limits := research.DefaultLimits
	limits.Timeout, limits.MaxRounds = settings.ResearchTimeout, settings.ResearchMaxRounds
	limits.MaxSearches, limits.MaxTokens = settings.ResearchMaxSearches, settings.ResearchMaxTokens
	meter := spending.NewSearchMeter(guard, ledger, pricing, spending.PurposeResearch, "tavily")
	runner := research.NewRunner(store, model, searcher, meter, guard, limits, logger)
	writer := "（调查模型自己写）"
	if settings.StrongLLMFor(config.StrongForResearch) { // 搜索和读网页用便宜的模型，结论交给更强的模型写
		runner.WriteConclusionsWith(spending.NewMeteredChatModel(newCloudChatModel(settings, claude, settings.StrongLLMModel, researchMaxTokens,
			settings.ResearchEffort, settings.StrongLLMThinking, 2, 0, httpClient), settings.StrongLLMModel, spending.PurposeResearch, pricing, ledger, logger))
		writer = settings.StrongLLMModel
	}
	go runner.Run(ctx)
	logger.Info("后台调查已开启", "model", settings.ResearchLLM, "conclusion_writer", writer, "timeout", limits.Timeout,
		"max_rounds", limits.MaxRounds, "max_searches", limits.MaxSearches, "max_tokens", limits.MaxTokens)
	return runner, func() { store.Close() }, nil
}

// summaryMaxTokens 是写滚动摘要时每次请求的输出上限。
const summaryMaxTokens = 600

// researchMaxTokens 是调查时每次请求的输出上限（结论比聊天长，聊天用 HARO_CLOUD_LLM_MAX_TOKENS）。
const researchMaxTokens = 1000

// buildSynthesizer 按 HARO_TTS 选择语音合成实现；piper 且没设地址时把 Piper 拉成子进程。
// 返回的清理函数总是非 nil。
func buildSynthesizer(ctx context.Context, settings config.Settings, httpClient *http.Client,
	logger *slog.Logger) (brain.SpeechSynthesizer, func(), error) {
	if settings.TTS == config.TTSGPTSoVITS {
		voice, err := speech.LoadGPTSoVITSVoice(settings.TTSVoiceFile)
		if err != nil {
			return nil, nil, err
		}
		logger.Info("语音合成：GPT-SoVITS", "url", settings.TTSURL, "voice", voice.Name,
			"emotions", len(voice.References), "streaming_mode", settings.TTSStreamingMode)
		return speech.NewGPTSoVITSSynthesizer(settings.TTSURL, settings.TTSRefDir, voice, settings.TTSStreamingMode,
			settings.TTSTimeout, httpClient), func() {}, nil
	}
	ttsURL, stop := settings.TTSURL, func() {}
	if ttsURL == "" {
		piper, err := speech.StartChildServer(ctx, speech.ChildServerSpec{
			Name: "piper", Command: settings.PiperPython,
			Args: []string{"-m", "piper.http_server", "--host", "127.0.0.1",
				"--port", strconv.Itoa(settings.PiperPort), "-m", settings.PiperModel},
			BaseURL: localURL(settings.PiperPort), ReadyPath: "/info", ReadyTimeout: 30 * time.Second,
		})
		if err != nil {
			return nil, nil, err
		}
		ttsURL, stop = piper.BaseURL, piper.Stop
		logger.Info("Piper 合成服务已就绪", "url", ttsURL)
	}
	return speech.NewPiperHTTPSynthesizer(ttsURL, settings.TTSTimeout, httpClient), stop, nil
}

// buildBrainOptions 读取性格档案；没设 HARO_PERSONAS_DIR 时不分性格。
func buildBrainOptions(settings config.Settings) (brain.Options, error) {
	// 基础提示词：内置的永远生效，HARO_SYSTEM_PROMPT 只能追加
	options := brain.Options{MaxHistoryTurn: settings.MaxHistoryTurn, Instructions: brain.ComposeInstructions(settings.ExtraSystemPrompt),
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

// activeModel 返回实际在用的模型名（日志用）：云端且有本地退路时两个都写。
func activeModel(settings config.Settings) string {
	if settings.LLMProvider != config.LLMCloud {
		if settings.LLMFallback {
			return settings.Model + "（退路 " + settings.CloudLLMModel + "）"
		}
		return settings.Model
	}
	if settings.LLMFallback {
		return settings.CloudLLMModel + "（退路 " + settings.Model + "）"
	}
	return settings.CloudLLMModel
}
