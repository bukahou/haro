// Package config 从环境变量读取 haro 的全部配置（通常放在一个 env 文件里，例如 haro.env，由启动脚本加载）。
// 设备名、地址一律可配，搬到树莓派只改环境变量，不改代码。
package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// 前台的模型从哪来（HARO_LLM_PROVIDER）。
const (
	LLMLocal = "local" // 本机 ollama
	LLMCloud = "cloud" // 云端（接口格式见 HARO_CLOUD_LLM_API）
)

// 云端模型的接口格式。
const (
	CloudAPIOpenAI    = "openai"    // OpenAI 兼容的 /chat/completions
	CloudAPIAnthropic = "anthropic" // Claude API 的 Messages 接口（官方 Go SDK）
)

// 语音合成引擎。
const (
	TTSPiper     = "piper"      // Piper 官方 HTTP 服务：开发用（不支持情绪、只会中文）
	TTSGPTSoVITS = "gpt-sovits" // GPT-SoVITS api_v2（按情绪选参考录音，流式）
)

// 拍照用哪套底层（都是要看的时候才打开摄像头）。
const (
	CameraBackendAuto         = "auto"         // 按系统选：Linux 用 mediadevices，其余用 ffmpeg
	CameraBackendMediaDevices = "mediadevices" // pion/mediadevices，预热到曝光稳定再取帧（目前只有 Linux）
	CameraBackendFFmpeg       = "ffmpeg"       // ffmpeg 抓一帧（备用；不预热）
)

// FrameSize 是「宽x高」写法的分辨率配置（例如 1280x720）。
type FrameSize struct{ Width, Height int }

// UnmarshalText 解析「宽x高」；写错时报错（启动失败），不悄悄换成默认值。
func (s *FrameSize) UnmarshalText(text []byte) error {
	widthText, heightText, found := strings.Cut(string(text), "x")
	width, widthErr := strconv.Atoi(widthText)
	height, heightErr := strconv.Atoi(heightText)
	if !found || widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return fmt.Errorf("分辨率要写成「宽x高」，例如 1280x720，现在是 %q", text)
	}
	s.Width, s.Height = width, height
	return nil
}

// Common 是终端和服务端都用的配置。
type Common struct {
	// RemovedSettingNotes 是环境里还写着、但已经删掉的配置项的提醒（不从环境变量读；启动时写进日志）
	RemovedSettingNotes []string
	OTLPEndpoint        string `env:"HARO_OTLP_ENDPOINT"` // OTLP/HTTP 根地址，如 http://collector:4318；空 = 不导出，只打日志
}

// ServerSettings 是服务端（haro-server）的配置：模型、代理、识别与语音合成服务、工具、费用、对话。
type ServerSettings struct {
	Common

	// 模型
	OllamaURL string `env:"HARO_OLLAMA_URL" envDefault:"http://127.0.0.1:11434"`
	Model     string `env:"HARO_MODEL" envDefault:"qwen3-vl:8b-instruct"` // 本地（ollama）模型；4b 是思考变体，每轮多等 1.5–3 秒
	// 等本地模型开始回答的上限：ollama 卸掉模型后的第一句要先从磁盘载入（冷启动十几秒），不能按等云端的几秒算，
	// 否则会被当成连不上、触发熔断
	LocalLLMTimeout time.Duration `env:"HARO_LOCAL_LLM_TIMEOUT" envDefault:"30s"`
	// 追加在前台的基础规则后面（前台的定义见 internal/agents/builtin/front.md）
	ExtraSystemPrompt string `env:"HARO_SYSTEM_PROMPT"`
	// 代理定义的覆盖目录：里面同名的 *.md 替换程序里带的那份（internal/agents/builtin/）；空 = 用程序里带的
	AgentsDir string `env:"HARO_AGENTS_DIR"`
	// 主人资料：household.md（全家）+ 每人一份 *.md（开头 front matter 写 person: <声纹名字>）；空 = 不用
	ProfileDir     string `env:"HARO_PROFILE_DIR"`
	MaxHistoryTurn int    `env:"HARO_MAX_HISTORY_TURN" envDefault:"6"`
	// 一轮里请求模型带的上下文（最大的那次请求的输入 token，模型报的）到了这么多，就提前把较早的几轮移出窗口、压缩（0 = 不看）。
	// 默认值是估算，没有实测：系统提示词加工具定义约 4k，最近 6 轮一般 5k–12k；24k 只在工具结果特别长时触发。
	// 待用账本里实际的 prompt_tokens 校准
	CompactContextTokens int `env:"HARO_CONTEXT_COMPACT_TOKENS" envDefault:"24000"`
	// 会破坏的操作（删记事等）问了主人之后，多久没确认就作废
	ConfirmationTimeout time.Duration `env:"HARO_CONFIRMATION_TIMEOUT" envDefault:"60s"`
	// 单次工具执行最多多久（所有工具共用，包括远程 MCP）；天气这类工具在这个期限里自己分配主力和备用的时间
	ToolTimeout time.Duration `env:"HARO_TOOL_TIMEOUT" envDefault:"10s"`
	// 不经模型、直接念的固定的话（每种语言一份，JSON）；空 = 用程序里带的那份
	PhrasesFile string `env:"HARO_PHRASES_FILE"`
	// 滚动摘要：掉出最近 N 轮的对话压成摘要放进提示词；false = 直接丢掉（旧行为）
	ConversationSummary bool `env:"HARO_CONVERSATION_SUMMARY" envDefault:"true"`

	// 服务端用哪个模型：local（ollama）/ cloud（云端）。两者不自动互相顶替：云端连不上时只保留最低能力（念「连不上」和现在的时间）
	LLMProvider string `env:"HARO_LLM_PROVIDER" envDefault:"cloud"`
	// 云端的接口格式：openai（OpenAI 兼容）/ anthropic（Claude API）
	CloudLLMAPI    string `env:"HARO_CLOUD_LLM_API" envDefault:"anthropic"`
	CloudLLMURL    string `env:"HARO_CLOUD_LLM_URL"`     // openai：接口的根地址（…/v1）；anthropic：留空 = 官方地址
	CloudLLMModel  string `env:"HARO_CLOUD_LLM_MODEL"`   // 云端的模型 ID（例如 claude-haiku-5-5）
	CloudLLMAPIKey string `env:"HARO_CLOUD_LLM_API_KEY"` // openai 的凭证：无默认值，只来自环境变量
	// openai：服务端要求的工具调用编号格式（opaque / kimi / mistral；空 = 不挑格式）。按服务显式写，不按模型名猜
	CloudLLMToolCallIDs string `env:"HARO_CLOUD_LLM_TOOL_CALL_IDS"`
	// anthropic 的凭证：开发机放 key；集群里留空，用 Workload Identity Federation（ANTHROPIC_FEDERATION_* 等，取值和检查在 llm 包），
	// 不放长期密钥。⚠️ 不用 ANTHROPIC_API_KEY 这个名字：Claude Code 也读它
	AnthropicAPIKey string `env:"HARO_ANTHROPIC_API_KEY"`
	// anthropic 的回答力度（low / medium / high；思考写法是 adaptive 时还可以 xhigh / max）：聊天和摘要求快，调查多花点力气
	CloudLLMEffort  string        `env:"HARO_CLOUD_LLM_EFFORT" envDefault:"low"`
	ResearchEffort  string        `env:"HARO_RESEARCH_EFFORT" envDefault:"medium"`
	CloudLLMTimeout time.Duration `env:"HARO_CLOUD_LLM_TIMEOUT" envDefault:"4s"`     // 等云端开始响应的上限，超过就当连不上（正常 2 s 内开始响应）
	CloudMaxTokens  int           `env:"HARO_CLOUD_LLM_MAX_TOKENS" envDefault:"800"` // 每次最多输出多少 token：要能装下约 300 字的详细回答
	// anthropic 的思考：disabled（关掉）/ between_tools（Sonnet 5.5 关掉思考的写法，它不接受 disabled）/ adaptive（思考关不掉的模型）
	CloudLLMThinking string `env:"HARO_CLOUD_LLM_THINKING" envDefault:"disabled"`

	// 更强的模型（可选）：按规则把几类请求交给它，其余仍用 HARO_CLOUD_LLM_MODEL；接口（HARO_CLOUD_LLM_API）与凭证和云端模型相同。
	// 空 = 不分，全部用云端模型。用在哪些地方（逗号分隔）：vision = 看摄像头画面；correction = 主人纠正、或明说要想清楚时由前台模型交给它重答；
	// research = 后台调查写结论。思考写法同 HARO_CLOUD_LLM_THINKING（默认按 Sonnet 5.5）；它出错或等太久时由云端模型接手
	StrongLLMModel    string        `env:"HARO_STRONG_LLM_MODEL"`
	StrongLLMUse      []string      `env:"HARO_STRONG_LLM_USE" envDefault:"vision,correction,research" envSeparator:","`
	StrongLLMEffort   string        `env:"HARO_STRONG_LLM_EFFORT" envDefault:"low"` // 调查写结论用 HARO_RESEARCH_EFFORT
	StrongLLMThinking string        `env:"HARO_STRONG_LLM_THINKING" envDefault:"between_tools"`
	StrongLLMTimeout  time.Duration `env:"HARO_STRONG_LLM_TIMEOUT" envDefault:"10s"` // 看画面 / 纠正时等它开始回答的上限（带图片比平时慢）
	// vision 用在哪：expert = 它当幕后专家看画面，把判断交给云端模型转述（前台始终是云端模型）；
	// takeover = 带画面的那次请求直接换它回答。都要 HARO_STRONG_LLM_USE 里有 vision
	StrongLLMVisionMode string `env:"HARO_STRONG_LLM_VISION_MODE" envDefault:"expert"`

	// 费用：用量账本与上限。汇率来自配置；云端模型的单价来自公共的模型价格目录（启动时拉取，见 pricecatalog）
	UsageDB  string  `env:"HARO_USAGE_DB"` // 用量账本（SQLite 文件）；空 = 只在内存里，重启清零
	USDToYen float64 `env:"HARO_USD_JPY"`  // 1 美元折多少日元；用到云端模型时必填
	// 模型价格目录（models.dev 的格式）：地址、取哪一家厂商的价格（目录里的 provider，和云端接口对应）、
	// 拉到的那份存在哪（下次拉不到时用；空 = 不存，拉不到就起不来）
	ModelPricesURL      string  `env:"HARO_MODEL_PRICES_URL" envDefault:"https://models.dev/api.json"`
	ModelPricesProvider string  `env:"HARO_MODEL_PRICES_PROVIDER" envDefault:"anthropic"`
	ModelPricesCache    string  `env:"HARO_MODEL_PRICES_CACHE"`
	ResearchDailyYen    float64 `env:"HARO_RESEARCH_DAILY_YEN" envDefault:"100"` // 后台调查每天上限；0 = 不限
	MonthlyYen          float64 `env:"HARO_MONTHLY_YEN"`                         // 每月总费用上限（大模型 + 搜索）；0 = 不限
	// 月度上限到了以后：主人口头确认一次提高多少（0 = 不能口头提高，到上限就停到下个月）；
	// 判断主人要不要提高的那次小调用每天最多几次（它不受额度管，靠这个封顶）
	MonthlyRaiseYen      float64 `env:"HARO_MONTHLY_RAISE_YEN" envDefault:"500"`
	OverBudgetDailyCalls int     `env:"HARO_OVER_BUDGET_DAILY_CALLS" envDefault:"30"`
	SearchDailyCredits   int     `env:"HARO_SEARCH_DAILY_CREDITS" envDefault:"30"` // 搜索积分每天上限（封在 Tavily 免费额度内）；0 = 不限
	SearchCreditUSD      float64 `env:"HARO_SEARCH_CREDIT_USD"`                    // 每个搜索积分的美元价；0 = 免费额度内

	// 语音识别：haro-asr 单独运行（模型等配置见 ASRSettings），服务端只认它的地址
	ASRURL string `env:"HARO_ASR_URL"` // 流式转写服务的根地址（协议照 OpenAI Realtime 的转写会话），例如 http://127.0.0.1:59126

	// 工具（见 docs/architecture.md）：自研工具内置在服务端里，MCP 只接第三方服务
	Tools   bool   `env:"HARO_TOOLS" envDefault:"true"`
	NotesDB string `env:"HARO_NOTES_DB"` // 记事本存储：SQLite 文件路径（服务端直接连）
	// 找记事时交给模型挑的最多条数（新的在前）；超过时结果里写明一共几条、只给了几条
	NotesListLimit int `env:"HARO_NOTES_LIST_LIMIT" envDefault:"200"`
	// 联网搜索：Tavily 官方远程 MCP。密钥不设 = 不提供联网搜索（凭证：无默认值，只来自环境变量）
	TavilyAPIKey string `env:"HARO_TAVILY_API_KEY"`
	TavilyMCPURL string `env:"HARO_TAVILY_MCP_URL" envDefault:"https://mcp.tavily.com/mcp/"`
	// 后台调查（有搜索时才有，只用云端模型）：任务账本、每个任务的上限
	ResearchDB          string        `env:"HARO_RESEARCH_DB"`                            // 任务账本（SQLite）；空 = 只在内存里
	ResearchTimeout     time.Duration `env:"HARO_RESEARCH_TIMEOUT" envDefault:"3m"`       // 搜索阶段最多多久
	ResearchMaxRounds   int           `env:"HARO_RESEARCH_MAX_ROUNDS" envDefault:"10"`    // 最多请求模型几轮（搜一次、读一次各占一轮；6 轮实测不够，2026-11-17 改 10）
	ResearchMaxSearches int           `env:"HARO_RESEARCH_MAX_SEARCHES" envDefault:"5"`   // 最多搜几次
	ResearchMaxTokens   int           `env:"HARO_RESEARCH_MAX_TOKENS" envDefault:"80000"` // 累计 token 上限
	// 天气：Open-Meteo 公开接口，不用密钥。家的位置：「纬度,经度」（只放在配置里，不进代码），
	// 或 "ip"（按家里的公网 IP 推定，城市级）；不设 = 查天气时必须说地名
	Weather             bool   `env:"HARO_WEATHER" envDefault:"true"`
	WeatherHome         string `env:"HARO_WEATHER_HOME"`
	WeatherHomeName     string `env:"HARO_WEATHER_HOME_NAME"`                                         // 家的名字；空 = 坐标时不写名字，IP 定位时用城市名
	WeatherIPLocateURL  string `env:"HARO_WEATHER_IP_LOCATE_URL" envDefault:"https://ipinfo.io/json"` // IP 定位服务（HTTPS，不用密钥）
	WeatherForecastURL  string `env:"HARO_WEATHER_FORECAST_URL" envDefault:"https://api.open-meteo.com/v1/forecast"`
	WeatherGeocodingURL string `env:"HARO_WEATHER_GEOCODING_URL" envDefault:"https://geocoding-api.open-meteo.com/v1/search"`
	// 按地名找坐标时用的语言（地名库返回的名字用这种语言写）
	WeatherGeocodingLanguage string `env:"HARO_WEATHER_GEOCODING_LANGUAGE" envDefault:"ja"`
	// Open-Meteo 查不到时的备用：jma = 日本气象厅网站的数据（只覆盖日本、按地区；地区按坐标自动找：
	// 国土地理院的地址反查 + 气象厅的地区表，所以要查的坐标也会发给国土地理院）；none = 不用备用
	WeatherBackup             string `env:"HARO_WEATHER_BACKUP" envDefault:"jma"`
	WeatherJMAURL             string `env:"HARO_WEATHER_JMA_URL" envDefault:"https://www.jma.go.jp/bosai"`
	WeatherReverseGeocoderURL string `env:"HARO_WEATHER_REVERSE_GEOCODER_URL" envDefault:"https://mreversegeocoder.gsi.go.jp/reverse-geocoder/LonLatToAddress"`

	// 语音合成：HARO_TTS 选引擎；服务都单独运行，服务端只认地址
	TTS        string        `env:"HARO_TTS" envDefault:"gpt-sovits"`  // piper / gpt-sovits
	TTSURL     string        `env:"HARO_TTS_URL"`                      // 语音合成服务的根地址（必填）
	TTSTimeout time.Duration `env:"HARO_TTS_TIMEOUT" envDefault:"30s"` // 单句合成（含边收边播）的超时
	// gpt-sovits：参考录音在服务那台机器上的目录；声音文件（各情绪的参考录音名与文字）；流式档位（2 = 中等，R-2）
	TTSRefDir        string `env:"HARO_TTS_REF_DIR"`
	TTSVoiceFile     string `env:"HARO_TTS_VOICE_FILE"`
	TTSStreamingMode int    `env:"HARO_TTS_STREAMING_MODE" envDefault:"2"`

	// 性格档案：目录下的 *.json；不设目录 = 不分性格（只用基础提示词）
	PersonasDir      string        `env:"HARO_PERSONAS_DIR"`
	PersonaDefault   string        `env:"HARO_PERSONA_DEFAULT" envDefault:"buddy"`  // 没记过当前性格时用哪个（ID）
	PersonaState     string        `env:"HARO_PERSONA_STATE"`                       // 记住当前性格的文件；空 = 重启回到默认
	ConversationsDB  string        `env:"HARO_CONVERSATIONS_DB"`                    // 对话库（SQLite 文件）：对话跟人走；空 = 只在内存里（服务端重启就没了）
	ConversationsDir string        `env:"HARO_CONVERSATIONS_DIR"`                   // 旧版每人一个 JSON 文件的目录：设了就在启动时导入对话库（导入后改名 .imported 留底）
	WakeArbitration  time.Duration `env:"HARO_WAKE_ARBITRATION" envDefault:"300ms"` // 多台终端同时听到唤醒词时等多久再选一台；0 = 不仲裁

	// 切句
	// 切句的长短按会念出来的词数算（中文一个汉字算一个词，英文一个单词算一个）；变量名沿用以前按字数算时的
	FirstSegmentMinRunes int `env:"HARO_FIRST_SEGMENT_MIN_RUNES" envDefault:"4"` // 0 = 第一句不在逗号处提前切
	MaxSegmentRunes      int `env:"HARO_MAX_SEGMENT_RUNES" envDefault:"40"`

	// 终端 ↔ 服务端：默认都在本机，服务端只监听本机；终端在别的机器上时改监听地址（只在可信网络里）
	ServerListen string `env:"HARO_SERVER_LISTEN" envDefault:"127.0.0.1:59130"` // haro-server 监听地址

	// 其他
	RequestTimeout time.Duration `env:"HARO_REQUEST_TIMEOUT" envDefault:"120s"`
}

// TerminalSettings 是终端（haro）的配置：说完判断与唤醒、设备、服务端地址。
type TerminalSettings struct {
	Common

	// 说完判断与 VAD（每次打开识别流时随 session.update 交给 haro-asr）
	// 说完判断：semantic_vad（默认，Smart Turn 判断语气；haro-asr 要配了 HARO_TURN_MODEL）/ server_vad（只等静音 HARO_VAD_SILENCE）
	TurnDetection string        `env:"HARO_TURN_DETECTION" envDefault:"semantic_vad"`
	TurnEagerness string        `env:"HARO_TURN_EAGERNESS" envDefault:"auto"` // semantic_vad：low / auto / high，判定没说完后最长等多久（数值见 asrserver.Timing）
	VADThreshold  float64       `env:"HARO_VAD_THRESHOLD" envDefault:"0.5"`
	VADSilence    time.Duration `env:"HARO_VAD_SILENCE" envDefault:"700ms"` // server_vad：静音持续多久算说完（定值；实测判定说完要 716–788 ms，即它加上 VAD 的判定延迟）
	VADPrefix     time.Duration `env:"HARO_VAD_PREFIX" envDefault:"300ms"`  // 识别时带上语音开始前的这段（定值，没有单独实测）
	EchoGuard     time.Duration `env:"HARO_ECHO_GUARD" envDefault:"300ms"`  // 播完后再静音这么久才恢复收音（实测音箱的余音会被当成一句话；300 ms 是定值）

	// 唤醒词（唯一的对话方式）
	WakePhrase string        `env:"HARO_WAKE_PHRASE" envDefault:"哈罗哈罗"` // 只用于提示；真正生效的是 haro-asr 的关键词文件
	WakeIdle   time.Duration `env:"HARO_WAKE_IDLE" envDefault:"40s"`    // 对话窗口：回答播完后安静这么久就休眠（20 秒太快，接着问时已经睡了），休眠时说一声
	BargeIn    bool          `env:"HARO_BARGE_IN" envDefault:"true"`    // 回答时喊唤醒词可以打断（需要唤醒词）

	// 硬件
	// 设备一律自动检测（规则见 internal/terminal/device_rules.go）；只在要手动指定时才设，值是各系统自己的设备 ID
	AudioSource  string `env:"HARO_AUDIO_SOURCE"`  // 空 = 自动：跟系统默认，但跳过蓝牙麦克风
	AudioSink    string `env:"HARO_AUDIO_SINK"`    // 空 = 跟系统默认（蓝牙重连后自动接回）
	CameraDevice string `env:"HARO_CAMERA_DEVICE"` // 空 = 自动找；none = 不用摄像头
	// 终端的提示音（服务端连不上 / 回来了）：目录里放 offline.wav、online.wav（16 bit PCM）；不设 = 内置的不含人声的提示音
	TonesDir string `env:"HARO_TONES_DIR"`
	// 拍照用哪套底层：auto / mediadevices / ffmpeg
	CameraBackend string `env:"HARO_CAMERA_BACKEND" envDefault:"auto"`
	// 想向摄像头要的分辨率；摄像头没有它时，改要宽度够的里面最小的（都不够宽就要最大的）
	CameraSize FrameSize `env:"HARO_CAMERA_SIZE" envDefault:"1280x720"`
	// 送给模型的画面最大宽度：画面更宽就缩到这么宽，更窄就原样送（不放大）。1280×720 约 1,200 token，640×480 约 400
	FrameWidth int `env:"HARO_FRAME_WIDTH" envDefault:"1280"`

	ServerURL string `env:"HARO_SERVER_URL" envDefault:"http://127.0.0.1:59130"` // 终端连服务端的地址
}

// parseSettings 从环境变量读配置，并检查还写着的已删除配置项（提醒记在 RemovedSettingNotes 里）。
// parseSettings 从环境变量读出一种配置，并记下环境里还写着、但已经删掉的配置项。
func parseSettings[T interface {
	*ServerSettings | *TerminalSettings
}](settings T, common *Common, side settingSide) error {
	if err := env.Parse(settings); err != nil {
		return err
	}
	notes, err := checkRemovedSettings(os.LookupEnv, side)
	common.RemovedSettingNotes = notes
	return err
}

// LoadBrainSettings 读取并校验服务端（haro-server）的配置。
func LoadBrainSettings() (ServerSettings, error) {
	var settings ServerSettings
	if err := parseSettings(&settings, &settings.Common, serverSide); err != nil {
		return settings, err
	}
	return settings, settings.validateBrain()
}

// LoadTerminalSettings 读取并校验终端（haro）的配置。
func LoadTerminalSettings() (TerminalSettings, error) {
	var settings TerminalSettings
	if err := parseSettings(&settings, &settings.Common, terminalSide); err != nil {
		return settings, err
	}
	return settings, settings.validateTerminal()
}

func (settings TerminalSettings) validateTerminal() error {
	if settings.TurnDetection != "server_vad" && settings.TurnDetection != "semantic_vad" {
		return fmt.Errorf("HARO_TURN_DETECTION 只能是 semantic_vad 或 server_vad，现在是 %q", settings.TurnDetection)
	}
	switch settings.CameraBackend {
	case CameraBackendAuto, CameraBackendMediaDevices, CameraBackendFFmpeg:
	default:
		return fmt.Errorf("HARO_CAMERA_BACKEND 只能是 %s、%s 或 %s，现在是 %q", CameraBackendAuto, CameraBackendMediaDevices,
			CameraBackendFFmpeg, settings.CameraBackend)
	}
	if settings.ServerURL == "" {
		return fmt.Errorf("必须设 HARO_SERVER_URL（haro-server 的地址）")
	}
	return nil
}

// validateBrain 校验服务端的配置。判断说完、唤醒词要用到的模型由服务端（haro-asr）加载，所以也在这里看。
func (settings ServerSettings) validateBrain() error {
	for _, validate := range []func() error{settings.validateSpeech, settings.validateModels, settings.validateWeather,
		settings.validateStorage, settings.validateLimits, settings.validateListen} {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

// validateSpeech 检查语音合成和语音识别服务。
func (settings ServerSettings) validateSpeech() error {
	if err := requireHTTPURL("HARO_TTS_URL", settings.TTSURL, "语音合成服务的地址；Piper 也要自己先运行起来，例如 python -m piper.http_server"); err != nil {
		return err
	}
	switch settings.TTS {
	case TTSPiper:
	case TTSGPTSoVITS:
		if settings.TTSRefDir == "" || settings.TTSVoiceFile == "" {
			return fmt.Errorf("HARO_TTS=gpt-sovits 时必须设 HARO_TTS_REF_DIR 和 HARO_TTS_VOICE_FILE")
		}
		if settings.TTSStreamingMode < 0 || settings.TTSStreamingMode > 3 {
			return fmt.Errorf("HARO_TTS_STREAMING_MODE 只能是 0–3，现在是 %d", settings.TTSStreamingMode)
		}
	default:
		return fmt.Errorf("HARO_TTS 只能是 %s 或 %s，现在是 %q", TTSPiper, TTSGPTSoVITS, settings.TTS)
	}
	if err := requireHTTPURL("HARO_ASR_URL", settings.ASRURL, "语音识别服务 haro-asr 的地址；haro-asr 单独运行，见 ASRSettings"); err != nil {
		return err
	}
	return nil
}

// validateModels 检查用哪种模型、云端模型和更强的模型配齐了没有、费用怎么折算。
func (settings ServerSettings) validateModels() error {
	switch settings.LLMProvider {
	case LLMLocal:
	case LLMCloud:
		if err := settings.cloudLLMReady(); err != nil {
			return fmt.Errorf("HARO_LLM_PROVIDER=cloud 时%w（或临时用 HARO_LLM_PROVIDER=local）", err)
		}
	default:
		return fmt.Errorf("HARO_LLM_PROVIDER 只能是 %s 或 %s，现在是 %q", LLMLocal, LLMCloud, settings.LLMProvider)
	}
	if settings.ResearchesWithCloud() {
		if err := settings.cloudLLMReady(); err != nil {
			return fmt.Errorf("开着后台调查（设了搜索的密钥）时%w", err)
		}
	}
	if settings.UsesCloudLLM() && (settings.USDToYen <= 0 || settings.ModelPricesURL == "" || settings.ModelPricesProvider == "") {
		return fmt.Errorf("用到云端模型时必须设 HARO_USD_JPY、HARO_MODEL_PRICES_URL、HARO_MODEL_PRICES_PROVIDER（费用上限要靠汇率和单价折算，v5 R5）")
	}
	if err := settings.validateStrongLLM(); err != nil {
		return err
	}
	return nil
}

// validateStorage 检查对话库、记事库。
func (settings ServerSettings) validateStorage() error {
	if settings.ConversationsDir != "" && settings.ConversationsDB == "" {
		return fmt.Errorf("设了 HARO_CONVERSATIONS_DIR（旧的对话文件，只用来导入）就要设 HARO_CONVERSATIONS_DB（对话库）")
	}
	if settings.Tools && settings.NotesDB == "" {
		return fmt.Errorf("HARO_TOOLS=true 时必须设 HARO_NOTES_DB（或设 HARO_TOOLS=false）")
	}
	if settings.Tools && settings.NotesListLimit <= 0 {
		return fmt.Errorf("HARO_NOTES_LIST_LIMIT 要大于 0（找记事时交给模型挑的最多条数），现在是 %d", settings.NotesListLimit)
	}
	return nil
}

// validateLimits 检查各种上限、时长不能是负数或零。
func (settings ServerSettings) validateLimits() error {
	if settings.MonthlyRaiseYen < 0 || settings.OverBudgetDailyCalls < 0 || settings.CompactContextTokens < 0 {
		return fmt.Errorf("HARO_MONTHLY_RAISE_YEN、HARO_OVER_BUDGET_DAILY_CALLS、HARO_CONTEXT_COMPACT_TOKENS 不能是负数（现在是 %v、%d、%d）",
			settings.MonthlyRaiseYen, settings.OverBudgetDailyCalls, settings.CompactContextTokens)
	}
	if settings.ToolTimeout <= 0 {
		return fmt.Errorf("HARO_TOOL_TIMEOUT 要大于 0（单次工具执行最多多久），现在是 %v", settings.ToolTimeout)
	}
	if settings.ConfirmationTimeout <= 0 {
		return fmt.Errorf("HARO_CONFIRMATION_TIMEOUT 要大于 0（会破坏的操作多久没确认就作废），现在是 %v", settings.ConfirmationTimeout)
	}
	return nil
}

// validateListen 检查服务端的监听地址。
func (settings ServerSettings) validateListen() error {
	if settings.ServerListen == "" {
		return fmt.Errorf("必须设 HARO_SERVER_LISTEN（haro-server 的监听地址）")
	}
	return nil
}

// requireHTTPURL 检查必填的服务地址：解析得出、是 http / https、有主机名（按结构判断，不比字符串前缀）。
func requireHTTPURL(name, value, what string) error {
	if value == "" {
		return fmt.Errorf("必须设 %s（%s）", name, what)
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%s 要是 http(s)://主机[:端口] 形式的地址，现在是 %q", name, value)
	}
	return nil
}

// cloudLLMReady 按接口格式检查云端模型的配置齐不齐；返回的说明接在「…时」后面。
func (s ServerSettings) cloudLLMReady() error {
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
	default:
		return fmt.Errorf("HARO_CLOUD_LLM_API 只能是 %s 或 %s，现在是 %q", CloudAPIOpenAI, CloudAPIAnthropic, s.CloudLLMAPI)
	}
	return nil
}

// UsesCloudLLM 判断会不会调用云端模型（主用云端，或开着后台调查）。
func (s ServerSettings) UsesCloudLLM() bool {
	return s.LLMProvider == LLMCloud || s.ResearchesWithCloud()
}

// ResearchesWithCloud 判断开没开后台调查（有联网搜索才有；调查只用云端模型）。
func (s ServerSettings) ResearchesWithCloud() bool {
	return s.Tools && s.TavilyAPIKey != ""
}

// WeatherHomePlace 解析 HARO_WEATHER_HOME（「纬度,经度」）；没设时 Set=false。
func (s ServerSettings) WeatherHomePlace() (home WeatherHome, err error) {
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
	StrongForCorrection = "correction" // 主人纠正、或明说要想清楚时，由前台模型交给它重答
	StrongForResearch   = "research"   // 后台调查写结论
)

// HARO_STRONG_LLM_VISION_MODE 的取值：看画面时更强的模型怎么用。
const (
	VisionByExpert   = "expert"   // 当幕后专家：看画面、需要时自己联网查，判断交给前台转述
	VisionByTakeover = "takeover" // 带画面的那次请求直接换它回答
)

// VisionExpert 判断看画面是否交给幕后专家（更强的模型）。
func (s ServerSettings) VisionExpert() bool {
	return s.StrongLLMFor(StrongForVision) && s.StrongLLMVisionMode == VisionByExpert
}

// VisionTakeover 判断带画面的请求是否直接换更强的模型回答。
func (s ServerSettings) VisionTakeover() bool {
	return s.StrongLLMFor(StrongForVision) && s.StrongLLMVisionMode == VisionByTakeover
}

// StrongLLMFor 判断更强的模型是否用在 use（StrongForVision 等）上。
func (s ServerSettings) StrongLLMFor(use string) bool {
	return s.StrongLLMModel != "" && slices.Contains(s.StrongLLMUse, use)
}

// validateStrongLLM 检查更强的模型的配置（回答力度、思考写法这类厂商的取值由各自的适配层检查，见 llm 包）。
func (s ServerSettings) validateStrongLLM() error {
	if s.StrongLLMVisionMode != VisionByExpert && s.StrongLLMVisionMode != VisionByTakeover {
		return fmt.Errorf("HARO_STRONG_LLM_VISION_MODE 只能是 %s 或 %s，现在是 %q", VisionByExpert, VisionByTakeover, s.StrongLLMVisionMode)
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
	if s.USDToYen <= 0 {
		return fmt.Errorf("设了 HARO_STRONG_LLM_MODEL 时必须设 HARO_USD_JPY（费用上限要靠它折算；单价从模型价格目录取）")
	}
	return nil
}

// validateWeather 检查天气相关的配置：家的位置、备用来源。
func (s ServerSettings) validateWeather() error {
	if _, err := s.WeatherHomePlace(); err != nil {
		return err
	}
	if s.WeatherBackup != WeatherBackupJMA && s.WeatherBackup != WeatherBackupNone {
		return fmt.Errorf("HARO_WEATHER_BACKUP 只能是 %s 或 %s，现在是 %q", WeatherBackupJMA, WeatherBackupNone, s.WeatherBackup)
	}
	return nil
}
