// haro-brain：haro 的大脑（架构见 docs/architecture.md）。
// 识别（haro-asr 子进程）→ 模型 → 工具 → 性格 → 语音合成（GPT-SoVITS 服务或 Piper），
// 经 WebSocket（Realtime 事件）为终端 haro 服务。一条连接一个会话（S-3）。
// 这里只做组装：读配置 → 创建各段实现 → 挂到 HTTP 服务上。
package main

import (
	"context"
	"errors"
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

	"haro/internal/brain"
	"haro/internal/brainserver"
	"haro/internal/config"
	"haro/internal/conversationstore"
	"haro/internal/llm"
	"haro/internal/netdiag"
	"haro/internal/notes"
	"haro/internal/persona"
	"haro/internal/realtimeproto"
	"haro/internal/research"
	"haro/internal/speech"
	"haro/internal/spending"
	"haro/internal/telemetry"
	"haro/internal/tools"
	"haro/internal/weather"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", "haro-brain")
	slog.SetDefault(logger)
	if err := run(logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("haro-brain 退出", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	settings, err := config.LoadBrainSettings()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.SetupTelemetry(ctx, settings.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer shutdownTelemetry(context.Background())

	httpClient := &http.Client{Timeout: settings.RequestTimeout}

	synthesizer, stopSynthesizer, err := buildSynthesizer(ctx, settings, httpClient, logger)
	if err != nil {
		return err
	}
	defer stopSynthesizer()

	recognizer, streaming, stopRecognizer, err := buildRecognizer(ctx, settings, httpClient, logger)
	if err != nil {
		return err
	}
	defer stopRecognizer()

	// 用量账本与额度（v5 R5）：先有刹车，再接搜索、后台调查
	ledger, err := spending.OpenLedger(ctx, settings.UsageDB)
	if err != nil {
		return err
	}
	defer ledger.Close()
	if settings.UsageDB == "" {
		logger.Warn("没设 HARO_USAGE_DB：用量只记在内存里，大脑重启后额度重新计算")
	}
	guard := spending.NewGuard(ledger, spending.Limits{ResearchDailyYen: settings.ResearchDailyYen,
		LLMMonthlyYen: settings.MonthlyYen, SearchDailyCredits: settings.SearchDailyCredits, WarnRatio: 0.8}, logger)
	logger.Info("费用上限", "research_daily_yen", settings.ResearchDailyYen, "monthly_yen", settings.MonthlyYen,
		"search_daily_credits", settings.SearchDailyCredits)
	briefers := []brain.Briefer{brain.ClockBriefer{}, guard} // 每轮补充给模型的即时情况：现在几号几点、额度提醒、任务板
	notices := brain.NewNoticeHub()                          // 给终端的提示（后台调查查完了让终端响一声）
	var toolbox brain.Toolbox
	if settings.Tools {
		// 自研工具内置（v5）：记事本直接连数据库，网络诊断在本进程里执行；MCP 只接第三方（现在还没有）
		store, err := notes.Open(ctx, settings.NotesDB)
		if err != nil {
			return err
		}
		defer store.Close()
		checker := netdiag.NewChecker(settings.NetdiagDNSName, settings.NetdiagInternetTarget)
		builtins := append(tools.NoteTools(store, logger), tools.NetworkDiagnostics(checker)...)
		if settings.Weather { // 天气（v5 E2 / R7 c）：Open-Meteo，不用密钥
			home := weatherHome(ctx, settings, httpClient, logger)
			builtins = append(builtins, tools.WeatherTools(weather.NewClient(settings.WeatherForecastURL,
				settings.WeatherGeocodingURL, httpClient), home, logger)...)
			if home != nil {
				briefers = append(briefers, weather.LocationNote{Home: home}) // 每轮告诉模型主人家大致在哪（城市级）
			}
		}
		if searcher := connectWebSearch(ctx, settings, httpClient, logger); searcher != nil {
			defer searcher.Close()
			meter := spending.NewSearchMeter(guard, ledger, pricingOf(settings), spending.PurposeSearch, "tavily")
			builtins = append(builtins, tools.WebSearchTools(searcher, meter, logger)...)
			runner, closeRunner, err := startResearch(ctx, settings, httpClient, searcher, ledger, guard, logger)
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
	model := buildChatModel(settings, httpClient, ledger, guard, spending.PurposeChat, logger)
	logger.Info("预热模型（本地第一次可能要几十秒；云端不用预热）", "model", activeModel(settings))
	if err := model.Warmup(ctx); err != nil {
		logger.Warn("模型预热失败，第一轮对话会比较慢", "err", err)
	}
	components := brain.Components{
		Recognizer: recognizer, Streaming: streaming, Model: model, Synthesizer: synthesizer, Tools: toolbox,
		Briefers: briefers, Notices: notices,
		NewSegmenter: func() brain.TextSegmenter {
			return speech.NewSentenceSegmenter(settings.FirstSegmentMinRunes, settings.MaxSegmentRunes)
		},
	}
	// 对话簿所有终端连接共用：换终端、重连、大脑重启都接得上（v4 D1）
	var conversationStore brain.ConversationStore
	if settings.ConversationsDir != "" {
		conversationStore = conversationstore.NewFileStore(settings.ConversationsDir)
	}
	components.Conversations = brain.NewConversationBook(conversationStore, options, logger)
	if settings.ConversationSummary { // 滚动摘要（v5 R4）：和主对话同一套模型，按 summary 用途记账
		components.Conversations.SummarizeWith(buildChatModel(settings, httpClient, ledger, guard, spending.PurposeSummary,
			logger.With("model_use", "summary")))
	}
	if settings.WakeArbitration > 0 { // 多台终端同时听到唤醒词：只让最合适的那台回答（v4 D2）
		components.WakeArbiter = brain.NewWakeArbiter(settings.WakeArbitration)
	}
	newSession := func() brainserver.Session { return brain.NewSession(components, options, logger) }

	listener, err := net.Listen("tcp", settings.BrainListen)
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
	logger.Info("haro-brain 启动", "listen", listener.Addr().String(), "model", activeModel(settings), "tts", settings.TTS,
		"personas", len(options.Personas))
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return ctx.Err()
}

// buildRecognizer 按 HARO_ASR 选择识别实现；local 模式把 haro-asr 作为子进程拉起。
// 返回一问一答的识别器、流式转写（voice-api 模式为 nil）和清理函数。
func buildRecognizer(ctx context.Context, settings config.Settings, httpClient *http.Client,
	logger *slog.Logger) (brain.SpeechRecognizer, brain.StreamingRecognizer, func(), error) {
	switch settings.ASR {
	case config.ASRVoiceAPI:
		return speech.NewVoiceAPIRecognizer(settings.VoiceAPIURL, httpClient), nil, func() {}, nil
	case config.ASROpenAI:
		return speech.NewOpenAITranscriptionRecognizer(settings.ASRURL, settings.ASRModelName, httpClient),
			speech.NewRealtimeTranscriptionClient(settings.ASRURL), func() {}, nil
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
	if settings.SpeakerModel != "" { // 声纹（v3 H-3）
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
		return nil, nil, nil, err
	}
	logger.Info("haro-asr 识别服务已就绪", "url", asr.BaseURL, "threads", settings.ASRThreads, "vad", settings.VADModel != "")
	return speech.NewOpenAITranscriptionRecognizer(asr.BaseURL, settings.ASRModelName, httpClient),
		speech.NewRealtimeTranscriptionClient(asr.BaseURL), asr.Stop, nil
}

func localURL(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

// brainHandler 是大脑对终端的全部 HTTP 入口：对话（WebSocket）+ 远程登记声纹（v4 C-3，转给 haro-asr）。
// 终端只认识大脑的地址；haro-asr 只监听本机（容器部署时在同一个容器里）。
func brainHandler(settings config.Settings, newSession brainserver.SessionFactory, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", brainserver.NewHandler(newSession, logger))
	if settings.ASR == config.ASRLocal {
		asrURL, _ := url.Parse(localURL(settings.ASRPort))
		mux.Handle("POST /v1/voiceprints/{name}", httputil.NewSingleHostReverseProxy(asrURL))
	}
	return mux
}

// buildChatModel 按 HARO_LLM_PROVIDER 组装大脑用的模型，HARO_LLM_FALLBACK 时另一个当退路：
//   - cloud：云端为主、本地 ollama 退路（D-A ②）
//   - local：本地 ollama 为主、云端退路（本地模型那台机器离线时自动改用云端）
//
// purpose 是记账用途：主对话（chat）与滚动摘要（summary，R4）各建一套，费用分开记。
func buildChatModel(settings config.Settings, httpClient *http.Client, ledger spending.Ledger, guard *spending.Guard,
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
	newCloud := func(client *http.Client) brain.ChatModel {
		return spending.NewMeteredChatModel(
			llm.NewOpenAIChatModel(settings.CloudLLMURL, settings.CloudLLMModel, settings.CloudLLMAPIKey, maxTokens, client),
			settings.CloudLLMModel, purpose, pricing, ledger, logger)
	}
	var paid, free brain.ChatModel // paid：平时用的那套（可能花钱）；free：月度额度用完后退回的本地模型（R5 c）
	switch {
	case settings.LLMProvider != config.LLMCloud && !settings.LLMFallback:
		logger.Info("大脑：本地模型", "model", settings.Model)
		paid = newLocal(httpClient)
		free = paid
	case settings.LLMProvider != config.LLMCloud:
		logger.Info("大脑：本地模型，失败退回云端", "model", settings.Model, "fallback", settings.CloudLLMModel)
		paid = llm.NewFallbackChatModel(newLocal(quickClient), "local", newCloud(httpClient), "cloud", settings.LLMCooldown, logger)
		free = newLocal(httpClient)
	case !settings.LLMFallback:
		logger.Info("大脑：云端模型（月度额度用完后不回答：没有本地退路）", "model", settings.CloudLLMModel)
		paid = newCloud(quickClient)
	default:
		logger.Info("大脑：云端模型，失败退回本地", "model", settings.CloudLLMModel, "fallback", settings.Model)
		paid = llm.NewFallbackChatModel(newCloud(quickClient), "cloud", newLocal(httpClient), "local", settings.LLMCooldown, logger)
		free = newLocal(httpClient)
	}
	return spending.NewBudgetedChatModel(paid, free, guard, logger)
}

// pricingOf 是把用量折成日元的单价（都来自配置）。
func pricingOf(settings config.Settings) spending.Pricing {
	return spending.Pricing{USDToYen: settings.USDToYen, SearchCreditUSD: settings.SearchCreditUSD,
		Models: map[string]spending.TokenPrice{settings.CloudLLMModel: {
			InputUSDPerMillion: settings.CloudLLMPriceIn, OutputUSDPerMillion: settings.CloudLLMPriceOut}}}
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

// connectWebSearch 连接联网搜索（v5 E3）；没设密钥或连不上时返回 nil：第三方服务出问题不影响大脑启动，只是没有联网搜索。
func connectWebSearch(ctx context.Context, settings config.Settings, httpClient *http.Client, logger *slog.Logger) *tools.TavilySearcher {
	if settings.TavilyAPIKey == "" {
		logger.Info("联网搜索：没设 HARO_TAVILY_API_KEY，不提供")
		return nil
	}
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	searcher, err := tools.ConnectTavily(connectCtx, settings.TavilyMCPURL, settings.TavilyAPIKey, httpClient, logger)
	if err != nil {
		logger.Warn("联网搜索：连不上 Tavily，这次不提供（重启大脑再试）", "err", err)
		return nil
	}
	return searcher
}

// startResearch 开启后台调查（v5 第 3 节）：任务账本、调查用的模型（R6）、上限（R2），并在后台开始处理队列。
func startResearch(ctx context.Context, settings config.Settings, httpClient *http.Client, searcher tools.WebSearcher,
	ledger spending.Ledger, guard *spending.Guard, logger *slog.Logger) (*research.Runner, func(), error) {
	store, err := research.OpenTaskStore(ctx, settings.ResearchDB)
	if err != nil {
		return nil, nil, err
	}
	if settings.ResearchDB == "" {
		logger.Warn("没设 HARO_RESEARCH_DB：调查任务只记在内存里，大脑重启后就忘了")
	}
	pricing := pricingOf(settings)
	var model brain.ChatModel
	if settings.ResearchLLM == config.LLMCloud {
		model = spending.NewMeteredChatModel(llm.NewOpenAIChatModel(settings.CloudLLMURL, settings.CloudLLMModel,
			settings.CloudLLMAPIKey, researchMaxTokens, httpClient), settings.CloudLLMModel, spending.PurposeResearch, pricing, ledger, logger)
	} else {
		model = spending.NewMeteredChatModel(llm.NewOllamaChatModel(settings.OllamaURL, settings.Model, settings.ContextSize,
			settings.ModelKeepAlive, httpClient), settings.Model, spending.PurposeResearch, pricing, ledger, logger)
	}
	limits := research.DefaultLimits
	limits.Timeout, limits.MaxRounds = settings.ResearchTimeout, settings.ResearchMaxRounds
	limits.MaxSearches, limits.MaxTokens = settings.ResearchMaxSearches, settings.ResearchMaxTokens
	meter := spending.NewSearchMeter(guard, ledger, pricing, spending.PurposeResearch, "tavily")
	runner := research.NewRunner(store, model, searcher, meter, guard, limits, logger)
	go runner.Run(ctx)
	logger.Info("后台调查已开启", "model", settings.ResearchLLM, "timeout", limits.Timeout, "max_rounds", limits.MaxRounds,
		"max_searches", limits.MaxSearches, "max_tokens", limits.MaxTokens)
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

// buildBrainOptions 读取性格档案（v3 第 2 步）；没设 HARO_PERSONAS_DIR 时不分性格。
func buildBrainOptions(settings config.Settings) (brain.Options, error) {
	options := brain.Options{MaxHistoryTurn: settings.MaxHistoryTurn, Instructions: settings.SystemPrompt}
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
