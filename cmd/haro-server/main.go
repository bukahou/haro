// haro-server：haro 的服务端（架构见 docs/architecture.md）。
// 识别（haro-asr，单独运行）→ 模型 → 工具 → 性格 → 语音合成（GPT-SoVITS 服务或 Piper），
// 经 WebSocket（Realtime 事件）为终端 haro 服务。一条连接一个会话。
// 这里只做组装：读配置 → 创建各段实现 → 挂到 HTTP 服务上。
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // 内置时区数据：镜像里没有 /usr/share/zoneinfo 时 TZ（例如 Asia/Tokyo）照样生效（集群里默认是 UTC）

	"github.com/bukahou/haro/internal/appserver"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/endpoint"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/money"
	"github.com/bukahou/haro/internal/persona"
	"github.com/bukahou/haro/internal/pricecatalog"
	"github.com/bukahou/haro/internal/readiness"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
	"github.com/bukahou/haro/internal/spending"
	"github.com/bukahou/haro/internal/telemetry"
	"github.com/bukahou/haro/internal/tools"
	"github.com/bukahou/haro/internal/weather"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	versionJSON := flag.Bool("json", false, "和 -version 一起用：输出一行 JSON（program、version、os、arch、go），给 CI 读")
	flag.Parse()
	if *showVersion {
		if err := buildinfo.WriteVersion(os.Stdout, "haro-server", *versionJSON); err != nil {
			fmt.Fprintln(os.Stderr, "打印版本号：", err)
			os.Exit(1)
		}
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", telemetry.ServiceServer)
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
	zone := config.LocalTimeZone(time.Now())
	logger.Info("时区：「今天」「这个月」（费用上限什么时候翻篇）、告诉模型的现在时间都按它算（用环境变量 TZ 改）",
		"zone", zone.Name, "utc_offset", zone.Offset, "TZ", zone.TZ)
	if err := checkClaudeSettings(settings); err != nil {
		return err
	}
	catalog, err := loadAgentDefinitions(settings, logger) // 代理的定义写错了就起不来：先读，免得连好一堆东西才发现
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 关停时依次收尾（后开的先关）：会话都结束之后才停后台调查、关数据库，最后发出遥测
	var shutdown shutdownSteps
	defer shutdown.run(logger)

	if err := startTelemetry(ctx, settings, &shutdown); err != nil {
		return err
	}
	deps, err := connectServices(ctx, settings, logger, &shutdown)
	if err != nil {
		return err
	}
	if deps.accounts, err = openAccounts(ctx, settings, logger, &shutdown); err != nil {
		return err
	}
	kit, err := buildTooling(ctx, settings, catalog, deps, logger, &shutdown)
	if err != nil {
		return err
	}
	components, options, err := assembleBrain(ctx, settings, catalog, deps, kit, logger, &shutdown)
	if err != nil {
		return err
	}
	entry := entrypoints{online: brainserver.NewOnlineTerminals()}
	if entry.app, err = buildAppAPI(ctx, settings, deps, kit, entry.online, logger); err != nil {
		return err
	}
	return serve(ctx, settings, components, options, deps, entry, logger)
}

// 关停各步的期限（加起来 10 秒，比 k8s 的宽限期 30 秒短）：停止接新连接、等普通请求 2 秒 → 会话收尾 5 秒 →
// 后台调查停下 2 秒 → 遥测发出 1 秒（telemetry.ShutdownTimeout，终端也用它）。
const (
	shutdownRequests  = 2 * time.Second
	shutdownSessions  = 5 * time.Second
	shutdownResearch  = 2 * time.Second
	shutdownReminders = 2 * time.Second
	shutdownMemory    = 2 * time.Second
)

// brainHandler 是服务端的全部 HTTP 入口：对话（WebSocket）+ 远程登记声纹（要读的句子和登记都转给 haro-asr）+ 存活 / 就绪检查。
// 终端只认识服务端的地址；haro-asr 只监听本机（集群里在同一个 Pod）。
func brainHandler(settings config.ServerSettings, newSession brainserver.SessionFactory, monitor *readiness.Monitor,
	entry entrypoints, logger *slog.Logger) (http.Handler, error) {
	asrURL, err := url.Parse(settings.ASRURL)
	if err != nil {
		return nil, fmt.Errorf("HARO_ASR_URL: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", brainserver.NewHandler(newSession, brainserver.Options{Online: entry.online}, logger))
	if entry.app != nil { // App 的接口（公网上只放行这一段；终端的 /v1/... 只在局域网）
		mux.Handle(appserver.PathPrefix, entry.app)
	}
	toASR := httputil.NewSingleHostReverseProxy(asrURL)
	mux.Handle(realtimeproto.VoiceprintRoute, toASR)
	mux.Handle(realtimeproto.EnrollmentScriptRoute, toASR)
	mux.HandleFunc("GET /livez", readiness.Alive)
	mux.Handle("GET /readyz", monitor)
	return mux, nil
}

// 就绪检查每 readinessInterval 一轮，每项最多等 readinessTimeout；k8s 的就绪探针只读结果。
const (
	readinessInterval = 30 * time.Second
	readinessTimeout  = 5 * time.Second
)

// readinessChecks 是服务端的就绪检查：识别、合成连得上，云端模型的凭证能用，账本写得进去，嵌入服务能用（配了的话）。
func readinessChecks(settings config.ServerSettings, deps services) ([]readiness.Check, error) {
	claude, ledger, httpClient := deps.claude, deps.ledger, deps.httpClient
	asrReady, err := endpoint.Join(settings.ASRURL, "/readyz", nil)
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
	if deps.embedder != nil { // 嵌入服务：真的嵌入一小段（服务在集群里，不花钱）
		checks = append(checks, readiness.Check{Name: "embedding", Probe: func(ctx context.Context) error {
			_, err := deps.embedder.EmbedQuery(ctx, "ping")
			return err
		}})
	}
	return checks, nil
}

// checkClaudeSettings 检查 Claude 特有的取值（回答力度、思考写法；取值由 llm 包定义）：用 Claude 的接口时才检查。
func checkClaudeSettings(settings config.ServerSettings) error {
	if settings.CloudLLMAPI != config.CloudAPIAnthropic {
		return nil
	}
	if err := llm.CheckClaudeCredentials(settings.AnthropicAPIKey); err != nil {
		return err
	}
	if err := llm.ValidateClaudeCacheTTL(settings.CloudLLMCacheTTL); err != nil {
		return fmt.Errorf("HARO_CLOUD_LLM_CACHE_TTL %w", err)
	}
	thinkings := []struct{ name, value string }{
		{"HARO_CLOUD_LLM_THINKING", settings.CloudLLMThinking}, {"HARO_STRONG_LLM_THINKING", settings.StrongLLMThinking},
	}
	if settings.ResearchThinking != "" {
		thinkings = append(thinkings, struct{ name, value string }{"HARO_RESEARCH_THINKING", settings.ResearchThinking})
	}
	for _, check := range thinkings {
		if err := llm.ValidateClaudeThinking(check.value); err != nil {
			return fmt.Errorf("%s %w", check.name, err)
		}
	}
	// 力度和它实际搭配的思考写法一起查：调查的力度两个代理都用（调查用云端模型的写法、审查用它那个模型的；
	// 设了 HARO_RESEARCH_THINKING 时两个都用它）
	reviewerThinking := settings.StrongLLMThinking
	if settings.ResearchReviewer == config.ReviewerBase {
		reviewerThinking = settings.CloudLLMThinking
	}
	for _, check := range []struct{ name, effort, thinking string }{
		{"HARO_CLOUD_LLM_EFFORT", settings.CloudLLMEffort, settings.CloudLLMThinking},
		{"HARO_STRONG_LLM_EFFORT", settings.StrongLLMEffort, settings.StrongLLMThinking},
		{"HARO_RESEARCH_EFFORT", settings.ResearchEffort, cmp.Or(settings.ResearchThinking, settings.CloudLLMThinking)},
		{"HARO_RESEARCH_EFFORT", settings.ResearchEffort, cmp.Or(settings.ResearchThinking, reviewerThinking)},
	} {
		if err := llm.ValidateClaudeEffort(check.effort, check.thinking); err != nil {
			return fmt.Errorf("%s %w", check.name, err)
		}
	}
	return nil
}

// newClaudeAPI 创建全进程共用的 Claude 客户端（HARO_CLOUD_LLM_API=anthropic 时）；其余情况为 nil。
func newClaudeAPI(settings config.ServerSettings, httpClient *http.Client) (*llm.ClaudeAPI, error) {
	if settings.CloudLLMAPI != config.CloudAPIAnthropic {
		return nil, nil
	}
	return llm.NewClaudeAPI(settings.AnthropicAPIKey, settings.CloudLLMURL, httpClient)
}

// cloudModelSpec 是一个云端模型的参数：同一个云端接口，不同用途（聊天、更强的模型、看画面、调查）各一套。
type cloudModelSpec struct {
	model             string
	maxTokens         int
	effort, thinking  string        // 只对 anthropic 有效
	cacheTTL          string        // 提示词缓存留多久（只对 anthropic 有效；空 = 5 分钟）
	retries           int           // SDK 在 429 / 5xx / 断线时自己重试几次
	firstTokenTimeout time.Duration // 等开始回答最多多久；0 = 不限
	requestTimeout    time.Duration // 整次请求最多多久；0 = 只受调用方的 context 管
}

// newCloudChatModel 按 HARO_CLOUD_LLM_API 创建云端的 model：openai = OpenAI 兼容接口（用 client），
// anthropic = Claude API（用共用的 claude）。
// 每次请求都套上观测（gen_ai.* 的 span 与指标）。
func newCloudChatModel(settings config.ServerSettings, claude *llm.ClaudeAPI, spec cloudModelSpec, client *http.Client) brain.ChatModel {
	if settings.CloudLLMAPI == config.CloudAPIAnthropic {
		return llm.WithRequestTelemetry(llm.NewAnthropicChatModel(llm.AnthropicOptions{API: claude, Model: spec.model,
			MaxTokens: spec.maxTokens, Effort: spec.effort, Thinking: spec.thinking, MaxRetries: spec.retries,
			FirstTokenTimeout: spec.firstTokenTimeout, RequestTimeout: spec.requestTimeout, CacheTTL: spec.cacheTTL}),
			llm.ProviderAnthropic, spec.model)
	}
	return llm.WithRequestTelemetry(llm.NewOpenAIChatModel(llm.OpenAIOptions{BaseURL: settings.CloudLLMURL, Model: spec.model,
		APIKey: settings.CloudLLMAPIKey, MaxTokens: spec.maxTokens, HTTPClient: client, ToolCallIDs: llm.ToolCallIDStyle(settings.CloudLLMToolCallIDs),
		MaxRetries: spec.retries, FirstTokenTimeout: spec.firstTokenTimeout, RequestTimeout: spec.requestTimeout}),
		llm.ProviderOpenAICompatible, spec.model)
}

// loadPricing 是把用量算成钱的单价表（美元）：云端模型（和设了的更强的模型）的单价从模型价格目录取（启动时拉取，拉不到用上次存的；
// 都没有、或目录里没有用到的模型就起不来 —— 费用上限要靠单价算），本地模型标成不花钱；搜索积分的价格来自配置。
// 不用云端模型时不拉目录。
func loadPricing(ctx context.Context, settings config.ServerSettings, client *http.Client, logger *slog.Logger) (spending.Pricing, error) {
	pricing := spending.Pricing{SearchCreditUSD: settings.SearchCreditUSD,
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
func weatherHome(ctx context.Context, settings config.ServerSettings, httpClient *http.Client, logger *slog.Logger) weather.HomeLocator {
	home, _ := settings.WeatherHomePlace()
	switch {
	case !home.Set:
		logger.Info("天气：没设家的位置（HARO_WEATHER_HOME），问天气时要说地名")
		return nil
	case home.ByIP:
		locator := weather.NewIPHome(settings.WeatherIPLocateURL, settings.WeatherHomeName, httpClient)
		if place, err := locator.Home(ctx, ""); err != nil {
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
func weatherForecaster(ctx context.Context, settings config.ServerSettings, primary *weather.OpenMeteoClient, home weather.HomeLocator,
	httpClient *http.Client, logger *slog.Logger) weather.Forecaster {
	if settings.WeatherBackup != config.WeatherBackupJMA {
		return primary
	}
	jma := weather.NewJMA(settings.WeatherJMAURL, settings.WeatherReverseGeocoderURL, httpClient)
	if home != nil {
		go func() {
			locateCtx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			place, err := home.Home(locateCtx, "") // 配置里的家对谁都一样
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

// webSearchService 是联网搜索服务（现在的实现是 Tavily）：搜索、读网页，记账时的名目，关停。
type webSearchService interface {
	tools.WebSearcher
	Name() string
	Close() error
}

// connectWebSearch 准备联网搜索（这时不连：用到时才连，断了自动重连）；没设密钥时返回 nil。
func connectWebSearch(settings config.ServerSettings, httpClient *http.Client, logger *slog.Logger) webSearchService {
	if settings.TavilyAPIKey == "" {
		logger.Info("联网搜索：没设 HARO_TAVILY_API_KEY，不提供")
		return nil
	}
	logger.Info("联网搜索：Tavily（用到时才连，断了自动重连）")
	return tools.NewTavilySearcher(settings.TavilyMCPURL, settings.TavilyAPIKey, httpClient, logger)
}

// buildSynthesizer 按 HARO_TTS 选择语音合成实现（服务都单独运行，只按 HARO_TTS_URL 连）。
func buildSynthesizer(settings config.ServerSettings, httpClient *http.Client, logger *slog.Logger) (brain.SpeechSynthesizer, error) {
	if settings.TTS == config.TTSGPTSoVITS {
		voice, err := speech.LoadGPTSoVITSVoice(settings.TTSVoiceFile)
		if err != nil {
			return nil, err
		}
		if err := checkVoiceEmotions(voice, settings.TTSVoiceFile); err != nil {
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

// checkVoiceEmotions 检查声音档案里每段参考录音的名字：要么是平常语气那段（speech.DefaultReference），要么是情绪清单里的
// （brain.ParseEmotion，清单只在 brain 定义）。拼错的那段永远选不到、那种语气一直念成平常的，起不来比悄悄不生效好。
func checkVoiceEmotions(voice speech.GPTSoVITSVoice, file string) error {
	for _, name := range slices.Sorted(maps.Keys(voice.References)) {
		if name == speech.DefaultReference {
			continue
		}
		if emotion, err := brain.ParseEmotion(name); err != nil || emotion == brain.EmotionDefault {
			return fmt.Errorf("声音档案 %s 的参考录音 %q 不是情绪清单里的名字，这段录音永远用不上：%v", file, name, err)
		}
	}
	return nil
}

// buildBrainOptions 读取性格档案；没设 HARO_PERSONAS_DIR 时不分性格。
func buildBrainOptions(settings config.ServerSettings) (brain.Options, error) {
	phrases, err := brain.LoadPhrases(settings.PhrasesFile) // 不经模型、直接念的固定的话；没配就用程序里带的
	if err != nil {
		return brain.Options{}, err
	}
	options := brain.Options{MaxHistoryTurn: settings.MaxHistoryTurn, CompactContextTokens: settings.CompactContextTokens,
		IdleCompactAfter:    settings.IdleCompactAfter,
		ConfirmationTimeout: settings.ConfirmationTimeout, ToolTimeout: settings.ToolTimeout, Phrases: phrases,
		MonthlyRaise: money.Of(settings.MonthlyRaise, settings.Currency())}
	if settings.PersonasDir == "" {
		return options, nil
	}
	personas, err := persona.LoadCatalog(settings.PersonasDir)
	if err != nil {
		return options, err
	}
	// 默认性格写错时起不来（不悄悄换成档案里的第一个）
	if !slices.ContainsFunc(personas, func(p brain.Persona) bool { return p.ID == settings.PersonaDefault }) {
		ids := make([]string, len(personas))
		for i, p := range personas {
			ids[i] = p.ID
		}
		return options, fmt.Errorf("HARO_PERSONA_DEFAULT=%q 不在性格档案（%s）里，档案里有：%s", settings.PersonaDefault,
			settings.PersonasDir, strings.Join(ids, "、"))
	}
	options.Personas, options.DefaultPersona = personas, settings.PersonaDefault
	if settings.PersonaState != "" {
		options.PersonaState = persona.NewFileState(settings.PersonaState)
	}
	return options, nil
}

// activeModel 返回实际在用的模型名（日志用）。
func activeModel(settings config.ServerSettings) string {
	if settings.LLMProvider == config.LLMLocal {
		return settings.Model
	}
	return settings.CloudLLMModel
}

// strongCooldown：接手的强模型出错或等太久之后，这么久内直接由前台的模型接手（沿用原来云端↔本地退路的冷却时间）。
const strongCooldown = 30 * time.Second

// unavailableRetry：连不上模型服务之后，这么久内不再去连（马上告诉主人连不上），之后放一个请求去试（沿用同一个冷却时间）。
const unavailableRetry = 30 * time.Second

// exchangeRates 是把账本的原始金额（服务商的美元，以前记的日元）换成默认货币的汇率：设了费用上限才要。
// 启动时拉一次欧洲央行的参考汇率（拉不到用上次存的；都没有就起不来，不按猜的汇率判断上限），之后定时重拉，随 ctx 结束。
func exchangeRates(ctx context.Context, settings config.ServerSettings, client *http.Client, logger *slog.Logger) (money.Converter, error) {
	if !settings.HasSpendingCaps() {
		return money.SameCurrencyOnly{}, nil
	}
	rates := money.NewECBRates(settings.ExchangeRatesURL, settings.ExchangeRatesCache, client, logger)
	if err := rates.Load(ctx); err != nil {
		return nil, fmt.Errorf("参考汇率: %w", err)
	}
	if settings.ExchangeRatesCache == "" {
		logger.Warn("没设 HARO_EXCHANGE_RATES_CACHE：参考汇率拉到的不存，下次拉不到时服务端起不来")
	}
	if _, err := rates.Convert(money.Sum{money.USD: money.MicrosPerUnit}, settings.Currency()); err != nil {
		return nil, fmt.Errorf("HARO_DEFAULT_CURRENCY=%s：参考汇率里没有这种货币: %w", settings.Currency(), err)
	}
	go rates.RunRefresh(ctx)
	return rates, nil
}
