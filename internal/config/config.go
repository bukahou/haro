// Package config 从环境变量读取 haro 的全部配置（通常放在一个 env 文件里，例如 haro.env，由启动脚本加载）。
// 设备名、地址一律可配，搬到树莓派只改环境变量，不改代码。
package config

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// 服务端的来源。
const (
	LLMLocal = "local" // 本机 ollama
	LLMCloud = "cloud" // 云端（接口格式见 HARO_CLOUD_LLM_API）
)

// 云端模型的接口格式（v7）。
const (
	CloudAPIOpenAI    = "openai"    // OpenAI 兼容的 /chat/completions（Bedrock、OpenRouter、vLLM……）
	CloudAPIAnthropic = "anthropic" // Claude API 的 Messages 接口（官方 Go SDK）
)

// claudeEfforts 是思考关掉时 Claude 接受的回答力度（xhigh / max 要开着思考）。
var claudeEfforts = map[string]bool{"": true, "low": true, "medium": true, "high": true}

// 轮流方式。
const (
	TurnHandsFree  = "hands-free"   // v2：按回车进入对话模式，VAD 自动判断说完
	TurnPushToTalk = "push-to-talk" // v1.5：每轮按一次开始、再按一次结束
)

// 语音识别方式。
const (
	ASRLocal    = "local"     // haro 拉起 haro-asr 子进程（SenseVoice，CPU）
	ASROpenAI   = "openai"    // 外部的 OpenAI 兼容识别服务
	ASRVoiceAPI = "voice-api" // 外部的 Whisper 转写服务（whisper.cpp 兼容的 HTTP 接口）
)

// 语音合成引擎。
const (
	TTSPiper     = "piper"      // Piper：开发用（不支持情绪、只会中文）；不设 HARO_TTS_URL 时由 haro 拉起子进程
	TTSGPTSoVITS = "gpt-sovits" // GPT-SoVITS api_v2（按情绪选参考录音，流式）
)

// 摄像头取帧方式。
const (
	CameraContinuous = "continuous" // ffmpeg 常驻，随时有最新一帧（摄像头一直开着）
	CameraOnDemand   = "on-demand"  // 每轮说完时现抓（约 0.8 秒，与识别并行）
)

// Settings 是 haro 的运行配置。
type Settings struct {
	// 模型
	OllamaURL      string `env:"HARO_OLLAMA_URL" envDefault:"http://127.0.0.1:11434"`
	Model          string `env:"HARO_MODEL" envDefault:"qwen3-vl:4b"`
	ContextSize    int    `env:"HARO_CONTEXT_SIZE" envDefault:"4096"`
	ModelKeepAlive string `env:"HARO_MODEL_KEEP_ALIVE" envDefault:"30m"`
	// 追加在内置基础提示词后面（v5 U2：内置的永远生效，外部只能追加；内置的见 brain.DefaultInstructions）
	ExtraSystemPrompt string `env:"HARO_SYSTEM_PROMPT"`
	// 主人资料（v5 U1–U5）：household.md（全家）+ 每人一份 *.md（开头 front matter 写 person: <声纹名字>）；空 = 不用
	ProfileDir     string `env:"HARO_PROFILE_DIR"`
	MaxHistoryTurn int    `env:"HARO_MAX_HISTORY_TURN" envDefault:"6"`
	// 滚动摘要（v5 R4）：掉出最近 N 轮的对话压成摘要放进提示词；false = 直接丢掉（旧行为）
	ConversationSummary bool `env:"HARO_CONVERSATION_SUMMARY" envDefault:"true"`

	// 服务端：local（ollama）/ cloud（云端，失败时这一轮退回 ollama）
	LLMProvider string `env:"HARO_LLM_PROVIDER" envDefault:"local"`
	// 云端的接口格式（v7）：openai（OpenAI 兼容）/ anthropic（Claude API）
	CloudLLMAPI    string `env:"HARO_CLOUD_LLM_API" envDefault:"openai"`
	CloudLLMURL    string `env:"HARO_CLOUD_LLM_URL"`     // openai：接口的根地址（…/v1）；anthropic：留空 = 官方地址
	CloudLLMModel  string `env:"HARO_CLOUD_LLM_MODEL"`   // 云端的模型 ID（例如 claude-haiku-5-5）
	CloudLLMAPIKey string `env:"HARO_CLOUD_LLM_API_KEY"` // openai 的凭证：无默认值，只来自环境变量
	// anthropic 的凭证（v7 D3）：开发机放 key；集群里留空，由 SDK 走 Workload Identity Federation（ANTHROPIC_FEDERATION_* 等），
	// 不放长期密钥。⚠️ 不用 ANTHROPIC_API_KEY 这个名字：Claude Code 也读它
	AnthropicAPIKey string `env:"HARO_ANTHROPIC_API_KEY"`
	// anthropic 的回答力度（思考是关掉的，只能 low / medium / high）：聊天和摘要求快，调查多花点力气
	CloudLLMEffort  string        `env:"HARO_CLOUD_LLM_EFFORT" envDefault:"low"`
	ResearchEffort  string        `env:"HARO_RESEARCH_EFFORT" envDefault:"medium"`
	CloudLLMTimeout time.Duration `env:"HARO_CLOUD_LLM_TIMEOUT" envDefault:"4s"`      // 等云端开始响应的上限，超过就退回本地（正常 2 s 内开始响应）
	CloudMaxTokens  int           `env:"HARO_CLOUD_LLM_MAX_TOKENS" envDefault:"800"`  // 每次最多输出多少 token：要能装下约 300 字的详细回答（2026-11-17 从 300 提高）
	LLMFallback     bool          `env:"HARO_LLM_FALLBACK" envDefault:"true"`         // cloud 失败时退回本地 ollama
	LLMCooldown     time.Duration `env:"HARO_LLM_FALLBACK_COOLDOWN" envDefault:"30s"` // cloud 失败后这么久内直接用本地（熔断）
	// anthropic 的思考：disabled（关掉）/ between_tools（Sonnet 5.5 关掉思考的写法，它不接受 disabled）/ adaptive（思考关不掉的模型）
	CloudLLMThinking string `env:"HARO_CLOUD_LLM_THINKING" envDefault:"disabled"`

	// 更强的模型（可选）：按规则把几类请求交给它，其余仍用 HARO_CLOUD_LLM_MODEL；接口（HARO_CLOUD_LLM_API）与凭证和云端模型相同。
	// 空 = 不分，全部用云端模型。用在哪些地方（逗号分隔）：vision = 带摄像头画面的请求；correction = 主人在纠正上一句；
	// research = 后台调查写结论。思考写法同 HARO_CLOUD_LLM_THINKING（默认按 Sonnet 5.5）；它出错或等太久时由云端模型接手
	StrongLLMModel    string        `env:"HARO_STRONG_LLM_MODEL"`
	StrongLLMUse      []string      `env:"HARO_STRONG_LLM_USE" envDefault:"vision,correction,research" envSeparator:","`
	StrongLLMEffort   string        `env:"HARO_STRONG_LLM_EFFORT" envDefault:"low"` // 调查写结论用 HARO_RESEARCH_EFFORT
	StrongLLMThinking string        `env:"HARO_STRONG_LLM_THINKING" envDefault:"between_tools"`
	StrongLLMTimeout  time.Duration `env:"HARO_STRONG_LLM_TIMEOUT" envDefault:"10s"` // 看画面 / 纠正时等它开始回答的上限（带图片比平时慢）

	// 费用（v5 R5）：用量账本与上限。价格、汇率都来自配置（云端价格会变，不写死在代码里）
	UsageDB          string  `env:"HARO_USAGE_DB"`               // 用量账本（SQLite 文件）；空 = 只在内存里，重启清零
	USDToYen         float64 `env:"HARO_USD_JPY"`                // 1 美元折多少日元；用到云端模型时必填
	CloudLLMPriceIn  float64 `env:"HARO_CLOUD_LLM_PRICE_INPUT"`  // 云端模型每百万输入 token 的美元价；用到云端时必填
	CloudLLMPriceOut float64 `env:"HARO_CLOUD_LLM_PRICE_OUTPUT"` // 同上，输出 token
	// 提示词缓存（v7，Claude）：读缓存 / 写缓存每百万 token 的美元价；不设 = 按普通输入价算（记多不记少）
	CloudLLMPriceCacheRead  float64 `env:"HARO_CLOUD_LLM_PRICE_CACHE_READ"`
	CloudLLMPriceCacheWrite float64 `env:"HARO_CLOUD_LLM_PRICE_CACHE_WRITE"`
	// 更强的模型的单价（设了 HARO_STRONG_LLM_MODEL 时输入、输出必填）
	StrongLLMPriceIn         float64 `env:"HARO_STRONG_LLM_PRICE_INPUT"`
	StrongLLMPriceOut        float64 `env:"HARO_STRONG_LLM_PRICE_OUTPUT"`
	StrongLLMPriceCacheRead  float64 `env:"HARO_STRONG_LLM_PRICE_CACHE_READ"`
	StrongLLMPriceCacheWrite float64 `env:"HARO_STRONG_LLM_PRICE_CACHE_WRITE"`
	ResearchDailyYen         float64 `env:"HARO_RESEARCH_DAILY_YEN" envDefault:"100"`  // 后台调查每天上限；0 = 不限
	MonthlyYen               float64 `env:"HARO_MONTHLY_YEN"`                          // 每月总费用上限（大模型 + 搜索）；0 = 不限（未定）
	SearchDailyCredits       int     `env:"HARO_SEARCH_DAILY_CREDITS" envDefault:"30"` // 搜索积分每天上限（封在 Tavily 免费额度内）；0 = 不限
	SearchCreditUSD          float64 `env:"HARO_SEARCH_CREDIT_USD"`                    // 每个搜索积分的美元价；0 = 免费额度内

	// 语音识别
	ASR          string `env:"HARO_ASR" envDefault:"local"` // local / openai / voice-api
	ASRBinary    string `env:"HARO_ASR_BIN"`                // local：haro-asr 可执行文件（make build 生成 bin/haro-asr）
	ASRModelDir  string `env:"HARO_ASR_MODEL_DIR"`          // local：SenseVoice 模型目录
	ASRThreads   int    `env:"HARO_ASR_THREADS" envDefault:"4"`
	ASRPort      int    `env:"HARO_ASR_PORT" envDefault:"59126"`
	ASRURL       string `env:"HARO_ASR_URL"` // openai：OpenAI 兼容识别服务的根地址
	ASRModelName string `env:"HARO_ASR_MODEL_NAME" envDefault:"sensevoice-small"`
	VoiceAPIURL  string `env:"HARO_VOICE_API_URL"` // voice-api：外部 Whisper 转写服务的地址

	// 轮流方式与 VAD
	TurnMode string `env:"HARO_TURN_MODE" envDefault:"hands-free"` // hands-free / push-to-talk
	VADModel string `env:"HARO_VAD_MODEL"`                         // local + hands-free：silero_vad.onnx 路径
	// 说完判断：semantic_vad（默认，Smart Turn 判断语气）/ server_vad（只等静音 HARO_VAD_SILENCE）
	TurnDetection string        `env:"HARO_TURN_DETECTION" envDefault:"semantic_vad"`
	TurnEagerness string        `env:"HARO_TURN_EAGERNESS" envDefault:"auto"` // semantic_vad：low 3s / auto 2s / high 1s 最长等待
	TurnModel     string        `env:"HARO_TURN_MODEL"`                       // local + semantic_vad：smart-turn-v3.2-cpu.onnx 路径
	VADThreshold  float64       `env:"HARO_VAD_THRESHOLD" envDefault:"0.5"`
	VADSilence    time.Duration `env:"HARO_VAD_SILENCE" envDefault:"700ms"` // 静音持续多久算说完
	VADPrefix     time.Duration `env:"HARO_VAD_PREFIX" envDefault:"300ms"`  // 识别时带上语音开始前的这段
	EchoGuard     time.Duration `env:"HARO_ECHO_GUARD" envDefault:"300ms"`  // 播完后再静音这么久才恢复收音

	// 唤醒词（只在 hands-free 模式下生效）
	WakeWord      bool          `env:"HARO_WAKE_WORD" envDefault:"true"`
	WakePhrase    string        `env:"HARO_WAKE_PHRASE" envDefault:"哈罗哈罗"`   // 只用于提示；真正生效的是关键词文件
	WakeWordsFile string        `env:"HARO_WAKE_WORDS_FILE"`                 // local：关键词文件（仓库 assets/wake-words.txt）
	KWSModelDir   string        `env:"HARO_KWS_MODEL_DIR"`                   // local：sherpa-onnx 唤醒词模型目录
	KWSThreshold  float64       `env:"HARO_KWS_THRESHOLD" envDefault:"0.25"` // 触发门槛（关键词文件里写了 #门槛 的以文件为准）
	KWSScore      float64       `env:"HARO_KWS_SCORE" envDefault:"1.0"`      // 加分（关键词文件里写了 :加分 的以文件为准）
	WakeIdle      time.Duration `env:"HARO_WAKE_IDLE" envDefault:"40s"`      // 对话窗口：回答播完后安静这么久就休眠（20 秒太快，接着问时已经睡了），休眠时说一声
	BargeIn       bool          `env:"HARO_BARGE_IN" envDefault:"true"`      // 回答时喊唤醒词可以打断（v3 I-1，需要唤醒词）

	// 声纹（v3 H-3）：只有登记过的人能唤醒、才会被回答。不设模型 = 关闭；设了但还没人登记 = 不过滤
	SpeakerModel     string  `env:"HARO_SPEAKER_MODEL"`                      // sherpa-onnx 说话人识别模型（CAM++）
	VoiceprintsDir   string  `env:"HARO_VOICEPRINTS_DIR"`                    // 登记的声纹（终端 haro -enroll 经服务端登记生成；生物特征，只在本地）
	SpeakerThreshold float64 `env:"HARO_SPEAKER_THRESHOLD" envDefault:"0.5"` // 相似度达到这个值才算认出来
	// 唤醒词只有 1 秒多，声纹算不稳（真机：同一人同一位置，整句 0.73、唤醒词 0.25–0.38）：唤醒用低一些的门槛（v3 BV）。
	// 只过了这个门槛的唤醒，接着说的话仍要达到 HARO_SPEAKER_THRESHOLD 才回答
	WakeSpeakerThreshold float64 `env:"HARO_WAKE_SPEAKER_THRESHOLD" envDefault:"0.2"`

	// 工具（见 docs/architecture.md）：自研工具内置在服务端里，MCP 只接第三方服务
	Tools   bool   `env:"HARO_TOOLS" envDefault:"true"`
	NotesDB string `env:"HARO_NOTES_DB"` // 记事本存储：SQLite 文件路径（v5 起服务端直接连，不再有 haro-notes 程序）
	// 联网搜索（v5 E3）：Tavily 官方远程 MCP。密钥不设 = 不提供联网搜索（凭证：无默认值，只来自环境变量）
	TavilyAPIKey string `env:"HARO_TAVILY_API_KEY"`
	TavilyMCPURL string `env:"HARO_TAVILY_MCP_URL" envDefault:"https://mcp.tavily.com/mcp/"`
	// 后台调查（v5 第 3 节；有搜索时才有）：任务账本、用哪个模型（R6）、每个任务的上限（R2）
	ResearchDB          string        `env:"HARO_RESEARCH_DB"`                            // 任务账本（SQLite）；空 = 只在内存里
	ResearchLLM         string        `env:"HARO_RESEARCH_LLM" envDefault:"cloud"`        // cloud / local：调查用哪个模型（R6：默认云端，本地 8B 调工具不可靠）
	ResearchTimeout     time.Duration `env:"HARO_RESEARCH_TIMEOUT" envDefault:"3m"`       // 搜索阶段最多多久
	ResearchMaxRounds   int           `env:"HARO_RESEARCH_MAX_ROUNDS" envDefault:"10"`    // 最多请求模型几轮（搜一次、读一次各占一轮；6 轮实测不够，2026-11-17 改 10）
	ResearchMaxSearches int           `env:"HARO_RESEARCH_MAX_SEARCHES" envDefault:"5"`   // 最多搜几次
	ResearchMaxTokens   int           `env:"HARO_RESEARCH_MAX_TOKENS" envDefault:"80000"` // 累计 token 上限
	// 天气（v5 E2 / R7 c）：Open-Meteo 公开接口，不用密钥。家的位置：「纬度,经度」（只放在配置里，不进代码），
	// 或 "ip"（按家里的公网 IP 推定，城市级）；不设 = 查天气时必须说地名
	Weather             bool   `env:"HARO_WEATHER" envDefault:"true"`
	WeatherHome         string `env:"HARO_WEATHER_HOME"`
	WeatherHomeName     string `env:"HARO_WEATHER_HOME_NAME"`                                         // 空 = 坐标时叫「家」，IP 定位时用城市名
	WeatherIPLocateURL  string `env:"HARO_WEATHER_IP_LOCATE_URL" envDefault:"https://ipinfo.io/json"` // IP 定位服务（HTTPS，不用密钥）
	WeatherForecastURL  string `env:"HARO_WEATHER_FORECAST_URL" envDefault:"https://api.open-meteo.com/v1/forecast"`
	WeatherGeocodingURL string `env:"HARO_WEATHER_GEOCODING_URL" envDefault:"https://geocoding-api.open-meteo.com/v1/search"`
	// Open-Meteo 查不到时的备用：jma = 日本气象厅网站的数据（只覆盖日本、按地区；地区按坐标自动找：
	// 国土地理院的地址反查 + 气象厅的地区表，所以要查的坐标也会发给国土地理院）；none = 不用备用
	WeatherBackup             string `env:"HARO_WEATHER_BACKUP" envDefault:"jma"`
	WeatherJMAURL             string `env:"HARO_WEATHER_JMA_URL" envDefault:"https://www.jma.go.jp/bosai"`
	WeatherReverseGeocoderURL string `env:"HARO_WEATHER_REVERSE_GEOCODER_URL" envDefault:"https://mreversegeocoder.gsi.go.jp/reverse-geocoder/LonLatToAddress"`
	// 网络诊断：v3 起内置在主程序里（V-4），不再启动 haro-netdiag
	NetdiagDNSName        string `env:"HARO_NETDIAG_DNS_NAME" envDefault:"www.cloudflare.com"`
	NetdiagInternetTarget string `env:"HARO_NETDIAG_INTERNET_TARGET" envDefault:"1.1.1.1:443"`

	// 语音合成：HARO_TTS 选引擎。piper 且没设 HARO_TTS_URL 时，由 haro 把 Piper 官方 HTTP 服务作为子进程拉起
	TTS         string        `env:"HARO_TTS" envDefault:"piper"` // piper / gpt-sovits
	PiperPython string        `env:"HARO_PIPER_PYTHON"`           // 装了 piper-tts 与 flask 的 venv 里的 python
	PiperModel  string        `env:"HARO_PIPER_MODEL"`
	PiperPort   int           `env:"HARO_PIPER_PORT" envDefault:"59125"`
	TTSURL      string        `env:"HARO_TTS_URL"`                      // 外部语音合成服务地址（gpt-sovits 必填）
	TTSTimeout  time.Duration `env:"HARO_TTS_TIMEOUT" envDefault:"30s"` // 单句合成（含边收边播）的超时
	// gpt-sovits：参考录音在服务那台机器上的目录；声音文件（各情绪的参考录音名与文字）；流式档位（2 = 中等，R-2）
	TTSRefDir        string `env:"HARO_TTS_REF_DIR"`
	TTSVoiceFile     string `env:"HARO_TTS_VOICE_FILE"`
	TTSStreamingMode int    `env:"HARO_TTS_STREAMING_MODE" envDefault:"2"`

	// 性格档案（v3 第 2 步）：目录下的 *.json；不设目录 = 不分性格（只用基础提示词）
	PersonasDir      string        `env:"HARO_PERSONAS_DIR"`
	PersonaDefault   string        `env:"HARO_PERSONA_DEFAULT" envDefault:"buddy"`  // 没记过当前性格时用哪个（ID）
	PersonaState     string        `env:"HARO_PERSONA_STATE"`                       // 记住当前性格的文件；空 = 重启回到默认
	ConversationsDir string        `env:"HARO_CONVERSATIONS_DIR"`                   // 对话跟人走（v4 D1）：每人一个文件；空 = 只在内存里（服务端重启就没了）
	WakeArbitration  time.Duration `env:"HARO_WAKE_ARBITRATION" envDefault:"300ms"` // 多台终端同时听到唤醒词时等多久再选一台（v4 D2 / C-2）；0 = 不仲裁

	// 切句
	FirstSegmentMinRunes int `env:"HARO_FIRST_SEGMENT_MIN_RUNES" envDefault:"4"` // 0 = 第一句不在逗号处提前切
	MaxSegmentRunes      int `env:"HARO_MAX_SEGMENT_RUNES" envDefault:"40"`

	// 硬件
	// 设备一律自动检测（规则见 internal/terminal/device_rules.go）；只在要手动指定时才设，值是各系统自己的设备 ID（P-2）
	AudioSource  string `env:"HARO_AUDIO_SOURCE"`  // 空 = 自动：跟系统默认，但跳过蓝牙麦克风
	AudioSink    string `env:"HARO_AUDIO_SINK"`    // 空 = 跟系统默认（蓝牙重连后自动接回）
	CameraDevice string `env:"HARO_CAMERA_DEVICE"` // 空 = 自动找；none = 不用摄像头
	// 终端的提示音（服务端连不上 / 回来了）：目录里放 offline.wav、online.wav（16 bit PCM）；不设 = 内置的不含人声的提示音
	TonesDir   string `env:"HARO_TONES_DIR"`
	CameraMode string `env:"HARO_CAMERA_MODE" envDefault:"continuous"`
	// 想向摄像头要的分辨率；Linux 上摄像头没有它时，改要宽度够的里面最小的（都不够宽就要最大的）
	CameraSize string `env:"HARO_CAMERA_SIZE" envDefault:"1280x720"`
	CameraFPS  int    `env:"HARO_CAMERA_FPS" envDefault:"2"`
	// 送给模型的画面最大宽度：画面更宽就缩到这么宽，更窄就原样送（不放大）。1280×720 约 1,200 token，640×480 约 400
	FrameWidth  int           `env:"HARO_FRAME_WIDTH" envDefault:"1280"`
	FrameMaxAge time.Duration `env:"HARO_FRAME_MAX_AGE" envDefault:"3s"`
	MinSpeech   time.Duration `env:"HARO_MIN_SPEECH" envDefault:"700ms"`

	// 终端 ↔ 服务端：默认都在本机，服务端只监听本机；终端在别的机器上时改监听地址（只在可信网络里）
	ServerListen string `env:"HARO_SERVER_LISTEN" envDefault:"127.0.0.1:59130"`     // haro-server 监听地址
	ServerURL    string `env:"HARO_SERVER_URL" envDefault:"http://127.0.0.1:59130"` // 终端连服务端的地址

	// 其他
	RequestTimeout time.Duration `env:"HARO_REQUEST_TIMEOUT" envDefault:"120s"`
	OTLPEndpoint   string        `env:"HARO_OTLP_ENDPOINT"` // OTLP/HTTP 根地址，如 http://collector:4318；空 = 不导出，只打日志
}

func parseSettings() (Settings, error) { return env.ParseAs[Settings]() }

// LoadBrainSettings 读取并校验服务端（haro-server）用到的配置：模型、识别、语音合成、工具。
func LoadBrainSettings() (Settings, error) {
	settings, err := parseSettings()
	if err != nil {
		return settings, err
	}
	return settings, settings.validateBrain()
}

// LoadTerminalSettings 读取并校验终端（haro）用到的配置：轮流方式、摄像头、服务端地址。
func LoadTerminalSettings() (Settings, error) {
	settings, err := parseSettings()
	if err != nil {
		return settings, err
	}
	return settings, settings.validateTerminal()
}

func (settings Settings) validateTerminal() error {
	switch settings.TurnMode {
	case TurnPushToTalk:
	case TurnHandsFree:
		if settings.TurnDetection != "server_vad" && settings.TurnDetection != "semantic_vad" {
			return fmt.Errorf("HARO_TURN_DETECTION 只能是 semantic_vad 或 server_vad，现在是 %q", settings.TurnDetection)
		}
	default:
		return fmt.Errorf("HARO_TURN_MODE 只能是 %s 或 %s，现在是 %q", TurnHandsFree, TurnPushToTalk, settings.TurnMode)
	}
	if settings.CameraMode != CameraContinuous && settings.CameraMode != CameraOnDemand {
		return fmt.Errorf("HARO_CAMERA_MODE 只能是 %s 或 %s，现在是 %q", CameraContinuous, CameraOnDemand, settings.CameraMode)
	}
	if settings.ServerURL == "" {
		return fmt.Errorf("必须设 HARO_SERVER_URL（haro-server 的地址）")
	}
	return nil
}

// validateBrain 校验服务端的配置。免按键、唤醒词要用到的模型由服务端（haro-asr）加载，所以 HARO_TURN_MODE 也在这里看。
func (settings Settings) validateBrain() error {
	switch settings.TTS {
	case TTSPiper:
		if settings.TTSURL == "" && (settings.PiperPython == "" || settings.PiperModel == "") {
			return fmt.Errorf("HARO_TTS=piper 且没有设 HARO_TTS_URL 时，必须设 HARO_PIPER_PYTHON 和 HARO_PIPER_MODEL")
		}
	case TTSGPTSoVITS:
		if settings.TTSURL == "" || settings.TTSRefDir == "" || settings.TTSVoiceFile == "" {
			return fmt.Errorf("HARO_TTS=gpt-sovits 时必须设 HARO_TTS_URL、HARO_TTS_REF_DIR 和 HARO_TTS_VOICE_FILE")
		}
		if settings.TTSStreamingMode < 0 || settings.TTSStreamingMode > 3 {
			return fmt.Errorf("HARO_TTS_STREAMING_MODE 只能是 0–3，现在是 %d", settings.TTSStreamingMode)
		}
	default:
		return fmt.Errorf("HARO_TTS 只能是 %s 或 %s，现在是 %q", TTSPiper, TTSGPTSoVITS, settings.TTS)
	}
	switch settings.LLMProvider {
	case LLMLocal:
	case LLMCloud:
		if err := settings.cloudLLMReady(); err != nil {
			return fmt.Errorf("HARO_LLM_PROVIDER=cloud 时%w（或临时用 HARO_LLM_PROVIDER=local）", err)
		}
	default:
		return fmt.Errorf("HARO_LLM_PROVIDER 只能是 %s 或 %s，现在是 %q", LLMLocal, LLMCloud, settings.LLMProvider)
	}
	switch settings.ASR {
	case ASRLocal:
		if settings.ASRBinary == "" || settings.ASRModelDir == "" {
			return fmt.Errorf("HARO_ASR=local 时必须设 HARO_ASR_BIN 和 HARO_ASR_MODEL_DIR")
		}
	case ASROpenAI:
		if settings.ASRURL == "" {
			return fmt.Errorf("HARO_ASR=openai 时必须设 HARO_ASR_URL")
		}
	case ASRVoiceAPI:
		if settings.VoiceAPIURL == "" {
			return fmt.Errorf("HARO_ASR=voice-api 时必须设 HARO_VOICE_API_URL")
		}
	default:
		return fmt.Errorf("HARO_ASR 只能是 %s / %s / %s，现在是 %q", ASRLocal, ASROpenAI, ASRVoiceAPI, settings.ASR)
	}
	switch settings.TurnMode {
	case TurnPushToTalk:
	case TurnHandsFree:
		if settings.ASR == ASRVoiceAPI {
			return fmt.Errorf("HARO_TURN_MODE=hands-free 需要流式转写，HARO_ASR 不能是 voice-api")
		}
		if settings.ASR == ASRLocal && settings.VADModel == "" {
			return fmt.Errorf("HARO_TURN_MODE=hands-free 且 HARO_ASR=local 时必须设 HARO_VAD_MODEL")
		}
		if settings.WakeWord && settings.ASR == ASRLocal && (settings.KWSModelDir == "" || settings.WakeWordsFile == "") {
			return fmt.Errorf("HARO_WAKE_WORD=true 且 HARO_ASR=local 时必须设 HARO_KWS_MODEL_DIR 和 HARO_WAKE_WORDS_FILE（或设 HARO_WAKE_WORD=false）")
		}
		switch settings.TurnDetection {
		case "server_vad":
		case "semantic_vad":
			if settings.ASR == ASRLocal && settings.TurnModel == "" {
				return fmt.Errorf("HARO_TURN_DETECTION=semantic_vad 且 HARO_ASR=local 时必须设 HARO_TURN_MODEL（或改用 server_vad）")
			}
		default:
			return fmt.Errorf("HARO_TURN_DETECTION 只能是 semantic_vad 或 server_vad，现在是 %q", settings.TurnDetection)
		}
	default:
		return fmt.Errorf("HARO_TURN_MODE 只能是 %s 或 %s，现在是 %q", TurnHandsFree, TurnPushToTalk, settings.TurnMode)
	}
	if err := settings.validateWeather(); err != nil {
		return err
	}
	if settings.ResearchLLM != LLMLocal && settings.ResearchLLM != LLMCloud {
		return fmt.Errorf("HARO_RESEARCH_LLM 只能是 %s 或 %s，现在是 %q", LLMLocal, LLMCloud, settings.ResearchLLM)
	}
	if settings.ResearchesWithCloud() {
		if err := settings.cloudLLMReady(); err != nil {
			return fmt.Errorf("后台调查用云端模型（HARO_RESEARCH_LLM=cloud）时%w（或改成 local）", err)
		}
	}
	if settings.UsesCloudLLM() && (settings.USDToYen <= 0 || settings.CloudLLMPriceIn <= 0 || settings.CloudLLMPriceOut <= 0) {
		return fmt.Errorf("用到云端模型时必须设 HARO_USD_JPY、HARO_CLOUD_LLM_PRICE_INPUT、HARO_CLOUD_LLM_PRICE_OUTPUT（费用上限要靠它们折算，v5 R5）")
	}
	if err := settings.validateStrongLLM(); err != nil {
		return err
	}
	if settings.SpeakerModel != "" && settings.VoiceprintsDir == "" {
		return fmt.Errorf("设了 HARO_SPEAKER_MODEL 时必须设 HARO_VOICEPRINTS_DIR（声纹放在哪）")
	}
	if settings.Tools && settings.NotesDB == "" {
		return fmt.Errorf("HARO_TOOLS=true 时必须设 HARO_NOTES_DB（或设 HARO_TOOLS=false）")
	}
	if settings.ServerListen == "" {
		return fmt.Errorf("必须设 HARO_SERVER_LISTEN（haro-server 的监听地址）")
	}
	return nil
}

// cloudLLMReady 按接口格式检查云端模型的配置齐不齐；返回的说明接在「…时」后面。
func (s Settings) cloudLLMReady() error {
	switch s.CloudLLMAPI {
	case CloudAPIOpenAI:
		if s.CloudLLMURL == "" || s.CloudLLMModel == "" {
			return fmt.Errorf("必须设 HARO_CLOUD_LLM_URL 和 HARO_CLOUD_LLM_MODEL")
		}
		if s.CloudLLMAPIKey == "" {
			return fmt.Errorf("必须填 HARO_CLOUD_LLM_API_KEY（放在环境变量里）")
		}
	case CloudAPIAnthropic:
		if s.CloudLLMModel == "" {
			return fmt.Errorf("必须设 HARO_CLOUD_LLM_MODEL（例如 claude-haiku-5-5）")
		}
		if s.AnthropicAPIKey == "" && !anthropicAmbientCredentials() {
			return fmt.Errorf("必须有 Claude 的凭证：开发机填 HARO_ANTHROPIC_API_KEY；集群里配 Workload Identity Federation（ANTHROPIC_FEDERATION_RULE_ID 等）")
		}
		if !claudeEfforts[s.CloudLLMEffort] || !claudeEfforts[s.ResearchEffort] {
			return fmt.Errorf("HARO_CLOUD_LLM_EFFORT、HARO_RESEARCH_EFFORT 只能是 low、medium 或 high（思考是关掉的），现在是 %q、%q",
				s.CloudLLMEffort, s.ResearchEffort)
		}
	default:
		return fmt.Errorf("HARO_CLOUD_LLM_API 只能是 %s 或 %s，现在是 %q", CloudAPIOpenAI, CloudAPIAnthropic, s.CloudLLMAPI)
	}
	return nil
}

// anthropicAmbientCredentials 判断不填 HARO_ANTHROPIC_API_KEY 时，SDK 能不能自己找到凭证：
// Workload Identity Federation（集群里），或 SDK 的标准变量 / 登录档案。
func anthropicAmbientCredentials() bool {
	for _, name := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE"} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// UsesCloudLLM 判断会不会调用云端模型（主用云端，或本地为主、失败退回云端，或后台调查用云端）。
func (s Settings) UsesCloudLLM() bool {
	return s.LLMProvider == LLMCloud || s.LLMFallback || s.ResearchesWithCloud()
}

// ResearchesWithCloud 判断后台调查会不会用云端模型（有联网搜索才有后台调查）。
func (s Settings) ResearchesWithCloud() bool {
	return s.Tools && s.TavilyAPIKey != "" && s.ResearchLLM == LLMCloud
}

// WeatherHomePlace 解析 HARO_WEATHER_HOME（「纬度,经度」）；没设时 Set=false。
func (s Settings) WeatherHomePlace() (home WeatherHome, err error) {
	if strings.TrimSpace(s.WeatherHome) == "" {
		return WeatherHome{}, nil
	}
	if strings.EqualFold(strings.TrimSpace(s.WeatherHome), WeatherHomeByIP) {
		return WeatherHome{Set: true, ByIP: true}, nil
	}
	parts := strings.Split(s.WeatherHome, ",")
	if len(parts) == 2 {
		latitude, errLat := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		longitude, errLon := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if errLat == nil && errLon == nil && latitude >= -90 && latitude <= 90 && longitude >= -180 && longitude <= 180 {
			return WeatherHome{Set: true, Latitude: latitude, Longitude: longitude}, nil
		}
	}
	return WeatherHome{}, fmt.Errorf("HARO_WEATHER_HOME 要写成「纬度,经度」（例如 35.68,139.76）或 ip，现在是 %q", s.WeatherHome)
}

// WeatherHome 是家的位置。
type WeatherHome struct {
	Set                 bool
	ByIP                bool // 按公网 IP 推定（HARO_WEATHER_HOME=ip）
	Latitude, Longitude float64
}

// WeatherHomeByIP 是 HARO_WEATHER_HOME 的特殊值：按公网 IP 定位。
const WeatherHomeByIP = "ip"

// HARO_WEATHER_BACKUP 的取值。
const (
	WeatherBackupJMA  = "jma"
	WeatherBackupNone = "none"
)

// HARO_STRONG_LLM_USE 的取值：更强的模型用在哪些地方。
const (
	StrongForVision     = "vision"     // 带摄像头画面的请求
	StrongForCorrection = "correction" // 主人在纠正上一句
	StrongForResearch   = "research"   // 后台调查写结论
)

// claudeThinking 是 HARO_CLOUD_LLM_THINKING / HARO_STRONG_LLM_THINKING 的取值。
var claudeThinking = map[string]bool{"disabled": true, "between_tools": true, "adaptive": true}

// StrongLLMFor 判断更强的模型是否用在 use（StrongForVision 等）上。
func (s Settings) StrongLLMFor(use string) bool {
	return s.StrongLLMModel != "" && slices.Contains(s.StrongLLMUse, use)
}

// validateStrongLLM 检查思考写法与更强的模型的配置。
func (s Settings) validateStrongLLM() error {
	if !claudeThinking[s.CloudLLMThinking] || !claudeThinking[s.StrongLLMThinking] {
		return fmt.Errorf("HARO_CLOUD_LLM_THINKING、HARO_STRONG_LLM_THINKING 只能是 disabled、between_tools 或 adaptive，现在是 %q、%q",
			s.CloudLLMThinking, s.StrongLLMThinking)
	}
	if s.StrongLLMModel == "" {
		return nil
	}
	for _, use := range s.StrongLLMUse {
		if use != StrongForVision && use != StrongForCorrection && use != StrongForResearch {
			return fmt.Errorf("HARO_STRONG_LLM_USE 只能是 %s、%s、%s 的组合（逗号分隔），现在有 %q", StrongForVision, StrongForCorrection, StrongForResearch, use)
		}
	}
	if err := s.cloudLLMReady(); err != nil {
		return fmt.Errorf("设了 HARO_STRONG_LLM_MODEL（它和云端模型用同一个接口）时%w", err)
	}
	if s.CloudLLMAPI == CloudAPIAnthropic && !claudeEfforts[s.StrongLLMEffort] {
		return fmt.Errorf("HARO_STRONG_LLM_EFFORT 只能是 low、medium 或 high，现在是 %q", s.StrongLLMEffort)
	}
	if s.USDToYen <= 0 || s.StrongLLMPriceIn <= 0 || s.StrongLLMPriceOut <= 0 {
		return fmt.Errorf("设了 HARO_STRONG_LLM_MODEL 时必须设 HARO_USD_JPY、HARO_STRONG_LLM_PRICE_INPUT、HARO_STRONG_LLM_PRICE_OUTPUT（费用上限要靠它们折算）")
	}
	return nil
}

// validateWeather 检查天气相关的配置：家的位置、备用来源。
func (s Settings) validateWeather() error {
	if _, err := s.WeatherHomePlace(); err != nil {
		return err
	}
	if s.WeatherBackup != WeatherBackupJMA && s.WeatherBackup != WeatherBackupNone {
		return fmt.Errorf("HARO_WEATHER_BACKUP 只能是 %s 或 %s，现在是 %q", WeatherBackupJMA, WeatherBackupNone, s.WeatherBackup)
	}
	return nil
}
