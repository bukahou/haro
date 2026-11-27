package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
)

// v7：云端用 Claude 时的配置检查（接口格式、模型）。凭证配没配齐由 llm 包查（服务端启动时）。
func TestCloudLLMReadyForAnthropic(t *testing.T) {
	base := ServerSettings{CloudLLMAPI: CloudAPIAnthropic, CloudLLMModel: "claude-haiku-5-5", AnthropicAPIKey: "k",
		CloudLLMEffort: "low", ResearchEffort: "medium"}
	if err := base.cloudLLMReady(); err != nil {
		t.Fatalf("配齐了应通过：%v", err)
	}

	cases := []struct {
		name   string
		change func(*ServerSettings)
		want   string
	}{
		{"没设模型", func(s *ServerSettings) { s.CloudLLMModel = "" }, "HARO_CLOUD_LLM_MODEL"},
		{"不认识的接口格式", func(s *ServerSettings) { s.CloudLLMAPI = "gemini" }, "HARO_CLOUD_LLM_API"},
	}
	for _, c := range cases {
		settings := base
		c.change(&settings)
		if err := settings.cloudLLMReady(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应报错且提到 %q，实际 %v", c.name, c.want, err)
		}
	}

}

// 天气的备用来源只认 jma / none（不设 = 不用备用：跟部署的地方有关，没有默认值）；家的位置写错照样报错。
func TestValidateWeather(t *testing.T) {
	for _, backup := range []string{WeatherBackupJMA, WeatherBackupNone, ""} {
		if err := (ServerSettings{WeatherBackup: backup, WeatherGeocodingLanguage: "ja"}).validateWeather(); err != nil {
			t.Errorf("%s 应通过：%v", backup, err)
		}
	}
	if err := (ServerSettings{WeatherBackup: "met", WeatherGeocodingLanguage: "ja"}).validateWeather(); err == nil ||
		!strings.Contains(err.Error(), "HARO_WEATHER_BACKUP") {
		t.Errorf("不认识的备用来源应报错：%v", err)
	}
	if err := (ServerSettings{WeatherBackup: WeatherBackupJMA, WeatherHome: "家", WeatherGeocodingLanguage: "ja"}).validateWeather(); err == nil ||
		!strings.Contains(err.Error(), "HARO_WEATHER_HOME") {
		t.Errorf("家的位置写错应报错：%v", err)
	}
}

// OpenAI 兼容接口的检查不变：地址、模型、key 都要有。
func TestCloudLLMReadyForOpenAI(t *testing.T) {
	settings := ServerSettings{CloudLLMAPI: CloudAPIOpenAI, CloudLLMURL: "https://example.com/v1", CloudLLMModel: "m", CloudLLMAPIKey: "k"}
	if err := settings.cloudLLMReady(); err != nil {
		t.Fatalf("配齐了应通过：%v", err)
	}
	settings.CloudLLMAPIKey = ""
	if err := settings.cloudLLMReady(); err == nil || !strings.Contains(err.Error(), "HARO_CLOUD_LLM_API_KEY") {
		t.Errorf("没有 key 应报错：%v", err)
	}
}

// 更强的模型：不设就不检查；设了要有单价、用途合法、和云端模型同一个接口能用。
func TestValidateStrongLLM(t *testing.T) {
	base := ServerSettings{CloudLLMAPI: CloudAPIAnthropic, CloudLLMModel: "claude-haiku-5-5", AnthropicAPIKey: "k",
		CloudLLMEffort: "low", ResearchEffort: "medium", CloudLLMThinking: "disabled", StrongLLMThinking: "between_tools",
		StrongLLMVisionMode: VisionByExpert}
	if err := base.validateStrongLLM(); err != nil {
		t.Fatalf("不设更强的模型应通过：%v", err)
	}
	strong := base
	strong.StrongLLMModel, strong.StrongLLMUse, strong.StrongLLMEffort = "claude-sonnet-5-5", []string{"vision", "correction", "research"}, "low"
	strong.USDToYen = 160
	if err := strong.validateStrongLLM(); err != nil {
		t.Fatalf("配齐了应通过：%v", err)
	}
	if !strong.StrongLLMFor(StrongForVision) || base.StrongLLMFor(StrongForVision) {
		t.Error("StrongLLMFor：设了模型且列在用途里才算")
	}
	takeover := strong
	takeover.StrongLLMVisionMode = VisionByTakeover
	noVision := strong
	noVision.StrongLLMUse = []string{"correction"}
	if !strong.VisionExpert() || strong.VisionTakeover() || !takeover.VisionTakeover() || takeover.VisionExpert() ||
		noVision.VisionExpert() || noVision.VisionTakeover() || base.VisionExpert() {
		t.Error("看画面的方式：用途里有 vision 时按 HARO_STRONG_LLM_VISION_MODE 二选一，没有 vision 或没设模型时都不算")
	}
	cases := []struct {
		name   string
		change func(*ServerSettings)
		want   string
	}{
		{"没设汇率", func(s *ServerSettings) { s.USDToYen = 0 }, "HARO_USD_JPY"},
		{"不认识的用途", func(s *ServerSettings) { s.StrongLLMUse = []string{"vision", "chat"} }, "HARO_STRONG_LLM_USE"},
		{"云端接口没配好", func(s *ServerSettings) { s.CloudLLMModel = "" }, "HARO_STRONG_LLM_MODEL"},
		{"看画面的方式不对", func(s *ServerSettings) { s.StrongLLMVisionMode = "both" }, "HARO_STRONG_LLM_VISION_MODE"},
	}
	for _, c := range cases {
		settings := strong
		c.change(&settings)
		if err := settings.validateStrongLLM(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应报错且提到 %q，实际 %v", c.name, c.want, err)
		}
	}
}

// 环境里还写着已删除的配置项：值和现在的行为一致时只提醒；要求已删除的行为时启动失败。
func TestRemovedSettings(t *testing.T) {
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { value, ok := env[name]; return value, ok }
	}
	notes, err := checkRemovedSettings(lookup(map[string]string{"HARO_TURN_MODE": "hands-free", "HARO_WAKE_WORD": "true",
		"HARO_MIN_SPEECH": "700ms", "HARO_CAMERA_MODE": "on-demand", "HARO_CAMERA_FPS": "2", "HARO_FRAME_MAX_AGE": "3s"}), terminalSide)
	if err != nil || len(notes) != 6 {
		t.Errorf("和现在一致的写法只提醒：notes=%v err=%v", notes, err)
	}
	for _, env := range []map[string]string{{"HARO_TURN_MODE": "push-to-talk"}, {"HARO_WAKE_WORD": "false"},
		{"HARO_CAMERA_MODE": "continuous"}} {
		if _, err := checkRemovedSettings(lookup(env), terminalSide); err == nil {
			t.Errorf("%v 要求已删除的行为，应启动失败", env)
		}
	}
	// 服务端不再拉起子进程：这些项写着什么值都只是多余（服务端按 HARO_ASR_URL、HARO_TTS_URL 连）
	notes, err = checkRemovedSettings(lookup(map[string]string{"HARO_ASR": "local", "HARO_ASR_BIN": "bin/haro-asr",
		"HARO_ASR_PORT": "59126", "HARO_PIPER_PYTHON": "python3", "HARO_PIPER_MODEL": "zh.onnx", "HARO_PIPER_PORT": "59125"}), serverSide)
	if err != nil || len(notes) != 6 {
		t.Errorf("拉起子进程用的配置项只提醒：notes=%v err=%v", notes, err)
	}
	notes, err = checkRemovedSettings(lookup(map[string]string{"HARO_NETDIAG_DNS_NAME": "www.cloudflare.com", "HARO_NETDIAG_INTERNET_TARGET": "1.1.1.1:443"}), serverSide)
	if err != nil || len(notes) != 2 {
		t.Errorf("网络诊断的配置项只提醒：notes=%v err=%v", notes, err)
	}
	notes, err = checkRemovedSettings(lookup(map[string]string{"HARO_CONTEXT_SIZE": "4096", "HARO_MODEL_KEEP_ALIVE": "30m"}), serverSide)
	if err != nil || len(notes) != 2 {
		t.Errorf("Ollama 那边配的两项只提醒：notes=%v err=%v", notes, err)
	}
	prices := map[string]string{}
	for _, side := range []string{"CLOUD", "STRONG"} {
		for _, kind := range []string{"INPUT", "OUTPUT", "CACHE_READ", "CACHE_WRITE"} {
			prices["HARO_"+side+"_LLM_PRICE_"+kind] = "1"
		}
	}
	if notes, err = checkRemovedSettings(lookup(prices), serverSide); err != nil || len(notes) != 8 {
		t.Errorf("手写的八个单价只提醒：notes=%v err=%v", notes, err)
	}
	// 自动换本地模型删了：退路的两项写什么都只提醒（本机终端的环境里还写着 true，不能让终端起不来）；
	// 调查只用云端：写 cloud 只提醒，写 local 要求已删除的行为，启动失败
	notes, err = checkRemovedSettings(lookup(map[string]string{"HARO_LLM_FALLBACK": "true", "HARO_LLM_FALLBACK_COOLDOWN": "30s",
		"HARO_RESEARCH_LLM": "cloud"}), serverSide)
	if err != nil || len(notes) != 3 {
		t.Errorf("退路和调查的配置项：notes=%v err=%v", notes, err)
	}
	if _, err := checkRemovedSettings(lookup(map[string]string{"HARO_RESEARCH_LLM": "local"}), serverSide); err == nil {
		t.Error("HARO_RESEARCH_LLM=local 要求已删除的行为，应启动失败")
	}
	if notes, err := checkRemovedSettings(lookup(nil), serverSide); err != nil || len(notes) != 0 {
		t.Errorf("没写时什么都不说：notes=%v err=%v", notes, err)
	}
	// 改了名的（HARO_BRAIN_*）、删掉的旧对话导入（HARO_CONVERSATIONS_DIR）：写着空值只提醒；写着值就是要已删除的行为，起不来，
	// 报错里写明是哪一项
	for _, c := range []struct {
		name string
		side settingSide
	}{{"HARO_BRAIN_URL", terminalSide}, {"HARO_BRAIN_LISTEN", serverSide}, {"HARO_CONVERSATIONS_DIR", serverSide}} {
		if notes, err := checkRemovedSettings(lookup(map[string]string{c.name: ""}), c.side); err != nil || len(notes) != 1 {
			t.Errorf("%s 写着空值只提醒：notes=%v err=%v", c.name, notes, err)
		}
		if _, err := checkRemovedSettings(lookup(map[string]string{c.name: "x"}), c.side); err == nil || !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s 写着值应起不来并写明：%v", c.name, err)
		}
	}
}

// 分辨率写成「宽x高」；写错时报错（启动失败），不悄悄换成默认值。
func TestFrameSizeParsing(t *testing.T) {
	var size FrameSize
	if err := size.UnmarshalText([]byte("1280x720")); err != nil || size != (FrameSize{Width: 1280, Height: 720}) {
		t.Errorf("1280x720 应解析成 1280、720：%+v %v", size, err)
	}
	for _, bad := range []string{"1280", "1280*720", "x720", "0x720", "1280x-1", "宽x高"} {
		if err := new(FrameSize).UnmarshalText([]byte(bad)); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}

// 拍照底层只认三个值；写错时启动失败。
func TestCameraBackendIsValidated(t *testing.T) {
	settings := terminalSettingsFrom(t, map[string]string{"HARO_TURN_DETECTION": "server_vad"})
	for _, backend := range []string{CameraBackendAuto, CameraBackendMediaDevices, CameraBackendFFmpeg} {
		settings.CameraBackend = backend
		if err := settings.validateTerminal(); err != nil {
			t.Errorf("%s 应该可以：%v", backend, err)
		}
	}
	settings.CameraBackend = "v4l2"
	if err := settings.validateTerminal(); err == nil {
		t.Error("不认识的拍照底层应报错")
	}
}

// 提前压缩的门槛：默认 80000（守每次请求的输入低于 10 万）；0 = 不看；负数起不来。
func TestCompactContextTokens(t *testing.T) {
	clearHaroEnv(t)
	minimal := map[string]string{"HARO_LLM_PROVIDER": "local", "HARO_TTS": "piper", "HARO_TTS_URL": "http://127.0.0.1:9",
		"HARO_ASR_URL": "http://127.0.0.1:9", "HARO_MODEL": "local-model", "HARO_TOOLS": "none", "HARO_SERVER_LISTEN": "127.0.0.1:0",
		"HARO_STRONG_LLM_MODEL": "", "HARO_TAVILY_API_KEY": "", "HARO_CONTEXT_COMPACT_TOKENS": "",
		"HARO_CONVERSATIONS_DB": filepath.Join(t.TempDir(), "conversations.db")}
	for name, value := range minimal {
		t.Setenv(name, value)
	}
	settings, err := LoadBrainSettings()
	if err != nil || settings.CompactContextTokens != 80000 {
		t.Fatalf("默认 80000：%d %v", settings.CompactContextTokens, err)
	}
	t.Setenv("HARO_CONTEXT_COMPACT_TOKENS", "0")
	if settings, err := LoadBrainSettings(); err != nil || settings.CompactContextTokens != 0 {
		t.Errorf("0 = 不看：%d %v", settings.CompactContextTokens, err)
	}
	t.Setenv("HARO_CONTEXT_COMPACT_TOKENS", "-1")
	if _, err := LoadBrainSettings(); err == nil || !strings.Contains(err.Error(), "HARO_CONTEXT_COMPACT_TOKENS") {
		t.Errorf("负数应起不来：%v", err)
	}
}

// clearHaroEnv 去掉跑测试那台机器环境里的全部 HARO_* 变量（测试结束后恢复）：读环境变量的测试不受开发机配置的影响
// （例如开发机上还写着的已删除配置项）。
func clearHaroEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "HARO_") {
			t.Setenv(name, "") // 登记测试结束后恢复原值
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// envNames 是 T 自己的字段（不含嵌入的 Common）对应的环境变量。
func envNames[T any]() map[string]bool {
	names := map[string]bool{}
	fields := reflect.TypeFor[T]()
	for i := range fields.NumField() {
		field := fields.Field(i)
		if field.Anonymous {
			continue
		}
		if name, _, _ := strings.Cut(field.Tag.Get("env"), ","); name != "" {
			names[name] = true
		}
	}
	return names
}

// 终端和服务端的配置各管各的：同一个环境变量只在一边定义（两边都用的放 Common）。
func TestServerAndTerminalSettingsDoNotOverlap(t *testing.T) {
	server, terminal := envNames[ServerSettings](), envNames[TerminalSettings]()
	if len(server) == 0 || len(terminal) == 0 {
		t.Fatalf("两边都应读得出环境变量：%d / %d", len(server), len(terminal))
	}
	for name := range terminal {
		if server[name] {
			t.Errorf("%s 在服务端和终端的配置里都有：两边都用的放 Common", name)
		}
	}
	if !server["HARO_ASR_URL"] || !terminal["HARO_SERVER_URL"] || server["HARO_TURN_DETECTION"] {
		t.Error("服务端连识别服务、终端连服务端；说完判断是终端交给识别流的设置")
	}
}

// 已删除配置项只查自己那边的：终端和服务端常共用一份 env，服务端的旧配置不能让终端起不来，反过来也一样。
func TestRemovedSettingsAreCheckedPerSide(t *testing.T) {
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { value, ok := env[name]; return value, ok }
	}
	env := lookup(map[string]string{"HARO_RESEARCH_LLM": "local", "HARO_CAMERA_MODE": "continuous"})
	if _, err := checkRemovedSettings(env, serverSide); err == nil || !strings.Contains(err.Error(), "HARO_RESEARCH_LLM") {
		t.Errorf("服务端的旧配置要求已删除的行为：服务端应起不来：%v", err)
	}
	if _, err := checkRemovedSettings(env, terminalSide); err == nil || !strings.Contains(err.Error(), "HARO_CAMERA_MODE") {
		t.Errorf("终端的旧配置要求已删除的行为：终端应起不来：%v", err)
	}
	if notes, err := checkRemovedSettings(lookup(map[string]string{"HARO_RESEARCH_LLM": "local", "HARO_LLM_FALLBACK": "true"}), terminalSide); err != nil || len(notes) != 0 {
		t.Errorf("服务端的配置项不该影响终端：notes=%v err=%v", notes, err)
	}
}

// serverSettingsFrom 按 values 解析服务端配置（没写的用默认值），不读跑测试那台机器的环境变量。
func serverSettingsFrom(t *testing.T, values map[string]string) ServerSettings {
	t.Helper()
	var settings ServerSettings
	if err := env.ParseWithOptions(&settings, env.Options{Environment: values}); err != nil {
		t.Fatal(err)
	}
	return settings
}

// 服务端的配置校验按领域逐项查：基线（本地模型、Piper、不开工具）通过；每改坏一项都起不来，报错写明是哪个变量。
func TestValidateBrainTable(t *testing.T) {
	base := localServerSettings(t)
	if err := base.validateBrain(); err != nil {
		t.Fatalf("基线应通过：%v", err)
	}
	notDir := filepath.Join(t.TempDir(), "profile.md")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*ServerSettings)
		want   string
	}{
		{"语音合成没有地址", func(s *ServerSettings) { s.TTSURL = "" }, "HARO_TTS_URL"},
		{"GPT-SoVITS 缺参考录音", func(s *ServerSettings) { s.TTS = TTSGPTSoVITS }, "HARO_TTS_REF_DIR"},
		{"语音识别没有地址", func(s *ServerSettings) { s.ASRURL = "" }, "HARO_ASR_URL"},
		{"不认识的模型来源", func(s *ServerSettings) { s.LLMProvider = "remote" }, "HARO_LLM_PROVIDER"},
		{"用云端却没设模型", func(s *ServerSettings) { s.LLMProvider, s.CloudLLMModel = LLMCloud, "" }, "HARO_CLOUD_LLM_MODEL"},
		{"用云端却没设汇率", func(s *ServerSettings) {
			s.LLMProvider, s.CloudLLMModel, s.AnthropicAPIKey, s.USDToYen = LLMCloud, "claude-haiku-5-5", "k", 0
		}, "HARO_USD_JPY"},
		{"开着记事却没有记事库", func(s *ServerSettings) { s.ToolGroups, s.NotesDB = nil, "" }, "HARO_NOTES_DB"},
		{"记事候选上限为零", func(s *ServerSettings) { s.ToolGroups, s.NotesDB, s.NotesListLimit = nil, "/tmp/notes.db", 0 }, "HARO_NOTES_LIST_LIMIT"},
		{"工具组写错", func(s *ServerSettings) { s.ToolGroups = []string{"memory", "wether"} }, "HARO_TOOLS"},
		{"原来的 true", func(s *ServerSettings) { s.ToolGroups = []string{"true"} }, "HARO_TOOLS"},
		{"none 和别的组一起写", func(s *ServerSettings) { s.ToolGroups = []string{"none", "weather"} }, "HARO_TOOLS"},
		{"用本地模型却没写模型名", func(s *ServerSettings) { s.LLMProvider, s.Model = LLMLocal, "" }, "HARO_MODEL"},
		{"设了嵌入地址却没写模型名", func(s *ServerSettings) { s.EmbeddingURL, s.EmbeddingModel = "http://127.0.0.1:9/v1", "" }, "HARO_EMBEDDING_MODEL"},
		{"嵌入服务的地址不是网址", func(s *ServerSettings) { s.EmbeddingURL = "haro-embedding:80" }, "HARO_EMBEDDING_URL"},
		{"设了嵌入服务却没有模型名", func(s *ServerSettings) { s.EmbeddingURL, s.EmbeddingModel = "http://127.0.0.1:8080/v1", "" }, "HARO_EMBEDDING_MODEL"},
		{"主人资料的目录不存在", func(s *ServerSettings) { s.ProfileDir = filepath.Join(t.TempDir(), "没有这个目录") }, "HARO_PROFILE_DIR"},
		{"主人资料的目录是个文件", func(s *ServerSettings) { s.ProfileDir = notDir }, "HARO_PROFILE_DIR"},
		{"没有监听地址", func(s *ServerSettings) { s.ServerListen = "" }, "HARO_SERVER_LISTEN"},
	}
	for _, c := range cases {
		settings := base
		c.change(&settings)
		if err := settings.validateBrain(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应起不来并写明 %s，实际 %v", c.name, c.want, err)
		}
	}
}

// localServerSettings 是只用本地模型、Piper、不开工具的服务端配置（默认值照配置里的写法解析）。
func localServerSettings(t *testing.T) ServerSettings {
	return serverSettingsFrom(t, map[string]string{"HARO_TTS": TTSPiper, "HARO_TTS_URL": "http://127.0.0.1:5000",
		"HARO_ASR_URL": "http://127.0.0.1:59126", "HARO_LLM_PROVIDER": LLMLocal, "HARO_MODEL": "local-model", "HARO_TOOLS": "none"})
}

// cloudServerSettings 是用云端模型、配齐了费用刹车（月度上限 + 账本）的服务端配置。
func cloudServerSettings(t *testing.T) ServerSettings {
	settings := localServerSettings(t)
	settings.LLMProvider, settings.CloudLLMModel, settings.AnthropicAPIKey, settings.USDToYen = LLMCloud, "claude-haiku-5-5", "k", 150
	settings.MonthlyYen, settings.UsageDB = 1000, "/tmp/usage.db"
	return settings
}

// 费用刹车：用到云端模型时月度上限必须设且大于 0（没有默认的「不限」）；有月度上限就要有账本；能口头提高就得能判断。
// 只用本地模型（不花钱）时可以不设月度上限。
func TestSpendingBrakes(t *testing.T) {
	cloud := cloudServerSettings(t)
	if err := cloud.validateBrain(); err != nil {
		t.Fatalf("用云端、配齐了月度上限和账本应通过：%v", err)
	}
	if err := localServerSettings(t).validateBrain(); err != nil {
		t.Errorf("只用本地模型时不设月度上限也行：%v", err)
	}
	research := localServerSettings(t)
	research.ToolGroups, research.TavilyAPIKey = []string{ToolGroupResearch}, "k"
	research.CloudLLMModel, research.AnthropicAPIKey, research.USDToYen = "claude-haiku-5-5", "k", 150
	for _, c := range []struct {
		name     string
		settings ServerSettings
		change   func(*ServerSettings)
		want     string
	}{
		{"用云端却没设月度上限", cloud, func(s *ServerSettings) { s.MonthlyYen = 0 }, "HARO_MONTHLY_YEN"},
		{"只开后台调查（也用云端）却没设月度上限", research, func(*ServerSettings) {}, "HARO_MONTHLY_YEN"},
		{"用云端、有上限却没有账本", cloud, func(s *ServerSettings) { s.UsageDB = "" }, "HARO_USAGE_DB"},
		{"本地模型设了月度上限却没有账本", localServerSettings(t), func(s *ServerSettings) { s.MonthlyYen = 500 }, "HARO_USAGE_DB"},
		{"能口头提高却一次都不判断", cloud, func(s *ServerSettings) { s.OverBudgetDailyCalls = 0 }, "HARO_OVER_BUDGET_DAILY_CALLS"},
	} {
		settings := c.settings
		c.change(&settings)
		if err := settings.validateBrain(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应起不来并写明 %s，实际 %v", c.name, c.want, err)
		}
	}
	noRaise := cloud
	noRaise.MonthlyRaiseYen, noRaise.OverBudgetDailyCalls = 0, 0
	if err := noRaise.validateBrain(); err != nil {
		t.Errorf("不让口头提高（HARO_MONTHLY_RAISE_YEN=0）时判断次数是 0 也行：%v", err)
	}
}

// 数值配置的范围：上限、单价不能是负数；时长、token、轮数要大于 0。每一项改坏都起不来，报错写明是哪个变量。
func TestNumericSettingRanges(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*ServerSettings)
	}{
		{"HARO_MONTHLY_YEN", func(s *ServerSettings) { s.MonthlyYen = -1 }},
		{"HARO_RESEARCH_DAILY_YEN", func(s *ServerSettings) { s.ResearchDailyYen = -1 }},
		{"HARO_MONTHLY_RAISE_YEN", func(s *ServerSettings) { s.MonthlyRaiseYen = -1 }},
		{"HARO_OVER_BUDGET_DAILY_CALLS", func(s *ServerSettings) { s.OverBudgetDailyCalls = -1 }},
		{"HARO_SEARCH_DAILY_CREDITS", func(s *ServerSettings) { s.SearchDailyCredits = -1 }},
		{"HARO_SEARCH_CREDIT_USD", func(s *ServerSettings) { s.SearchCreditUSD = -0.01 }},
		{"HARO_USD_JPY", func(s *ServerSettings) { s.USDToYen = -1 }},
		{"HARO_TTS_TIMEOUT", func(s *ServerSettings) { s.TTSTimeout = 0 }},
		{"HARO_MAX_SEGMENT_RUNES", func(s *ServerSettings) { s.MaxSegmentRunes = 0 }},
		{"HARO_FIRST_SEGMENT_MIN_RUNES", func(s *ServerSettings) { s.FirstSegmentMinRunes = -1 }},
		{"HARO_LOCAL_LLM_TIMEOUT", func(s *ServerSettings) { s.LocalLLMTimeout = 0 }},
		{"HARO_CLOUD_LLM_TIMEOUT", func(s *ServerSettings) { s.CloudLLMTimeout = 0 }},
		{"HARO_CLOUD_LLM_MAX_TOKENS", func(s *ServerSettings) { s.CloudMaxTokens = 0 }},
		{"HARO_STRONG_LLM_TIMEOUT", func(s *ServerSettings) { s.StrongLLMTimeout = -time.Second }},
		{"HARO_REQUEST_TIMEOUT", func(s *ServerSettings) { s.RequestTimeout = 0 }},
		{"HARO_MAX_HISTORY_TURN", func(s *ServerSettings) { s.MaxHistoryTurn = -1 }},
		{"HARO_CONTEXT_COMPACT_TOKENS", func(s *ServerSettings) { s.CompactContextTokens = -1 }},
		{"HARO_CONTEXT_IDLE_COMPACT", func(s *ServerSettings) { s.IdleCompactAfter = -time.Minute }},
		{"HARO_CONFIRMATION_TIMEOUT", func(s *ServerSettings) { s.ConfirmationTimeout = -time.Second }},
		{"HARO_TOOL_TIMEOUT", func(s *ServerSettings) { s.ToolTimeout = -time.Second }},
		{"HARO_WAKE_ARBITRATION", func(s *ServerSettings) { s.WakeArbitration = -time.Millisecond }},
		{"HARO_RESEARCH_TIMEOUT", func(s *ServerSettings) { s.ResearchTimeout = -time.Second }},
		{"HARO_RESEARCH_MAX_ROUNDS", func(s *ServerSettings) { s.ResearchMaxRounds = -1 }},
		{"HARO_RESEARCH_MAX_SEARCHES", func(s *ServerSettings) { s.ResearchMaxSearches = -1 }},
		{"HARO_RESEARCH_MAX_TOKENS", func(s *ServerSettings) { s.ResearchMaxTokens = -1 }},
		{"HARO_RESEARCH_MAX_CALL_TOKENS", func(s *ServerSettings) { s.ResearchMaxCallTokens = -1 }},
		{"HARO_RESEARCH_WRITEUP_TIMEOUT", func(s *ServerSettings) { s.ResearchWriteUpTimeout = -time.Second }},
		{"HARO_RESEARCH_RECHECK_TIMEOUT", func(s *ServerSettings) { s.ResearchRecheckTimeout = -time.Second }},
		{"HARO_RESEARCH_REVIEW_TIMEOUT", func(s *ServerSettings) { s.ResearchReviewTimeout = -time.Second }},
		{"HARO_RESEARCH_MAX_OUTPUT_TOKENS", func(s *ServerSettings) { s.ResearchMaxOutputTokens = -1 }},
		{"HARO_RESEARCH_REVIEWER", func(s *ServerSettings) { s.ResearchReviewer = "sonnet" }},
	} {
		settings := cloudServerSettings(t)
		c.change(&settings)
		if err := settings.validateBrain(); err == nil || !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s 超出范围应起不来并写明：%v", c.name, err)
		}
	}
}

// 后台调查有没有审查：HARO_STRONG_LLM_USE 有 research；审查用更强的模型时还要设了它，改用云端模型时不用。
func TestResearchReviewed(t *testing.T) {
	cases := []struct {
		strong, reviewer string
		uses             []string
		want             bool
	}{
		{"strong-model", ReviewerStrong, []string{StrongForResearch}, true},
		{"", ReviewerStrong, []string{StrongForResearch}, false},
		{"", ReviewerBase, []string{StrongForResearch}, true},
		{"strong-model", ReviewerBase, []string{StrongForVision}, false},
	}
	for _, c := range cases {
		settings := ServerSettings{StrongLLMModel: c.strong, ResearchReviewer: c.reviewer, StrongLLMUse: c.uses}
		if got := settings.ResearchReviewed(); got != c.want {
			t.Errorf("%+v：有审查应为 %v", c, c.want)
		}
	}
}

// 写明了「0 = 不限 / 关掉 / 用程序里的默认值」的项，写 0 照常启动。
func TestZeroMeansUnlimitedOffOrDefault(t *testing.T) {
	for name, change := range map[string]func(*ServerSettings){
		"HARO_RESEARCH_DAILY_YEN（不限）":            func(s *ServerSettings) { s.ResearchDailyYen = 0 },
		"HARO_SEARCH_DAILY_CREDITS（不限）":          func(s *ServerSettings) { s.SearchDailyCredits = 0 },
		"HARO_SEARCH_CREDIT_USD（免费额度内）":          func(s *ServerSettings) { s.SearchCreditUSD = 0 },
		"HARO_MONTHLY_RAISE_YEN（不能口头提高）":         func(s *ServerSettings) { s.MonthlyRaiseYen = 0 },
		"HARO_CONTEXT_COMPACT_TOKENS（不看）":        func(s *ServerSettings) { s.CompactContextTokens = 0 },
		"HARO_WAKE_ARBITRATION（不仲裁）":             func(s *ServerSettings) { s.WakeArbitration = 0 },
		"HARO_FIRST_SEGMENT_MIN_RUNES（不提前切）":     func(s *ServerSettings) { s.FirstSegmentMinRunes = 0 },
		"HARO_CONFIRMATION_TIMEOUT（程序里的默认值）":     func(s *ServerSettings) { s.ConfirmationTimeout = 0 },
		"HARO_TOOL_TIMEOUT（程序里的默认值）":             func(s *ServerSettings) { s.ToolTimeout = 0 },
		"HARO_RESEARCH_*（程序里的默认值）":               func(s *ServerSettings) { s.ResearchTimeout, s.ResearchMaxRounds = 0, 0 },
		"HARO_RESEARCH_MAX_SEARCHES、TOKENS（默认值）": func(s *ServerSettings) { s.ResearchMaxSearches, s.ResearchMaxTokens = 0, 0 },
		"HARO_RESEARCH_*_TIMEOUT（默认值）": func(s *ServerSettings) {
			s.ResearchWriteUpTimeout, s.ResearchRecheckTimeout, s.ResearchReviewTimeout = 0, 0, 0
		},
		"HARO_RESEARCH_MAX_OUTPUT_TOKENS（代理定义的）": func(s *ServerSettings) { s.ResearchMaxOutputTokens = 0 },
	} {
		settings := cloudServerSettings(t)
		change(&settings)
		if err := settings.validateBrain(); err != nil {
			t.Errorf("%s 写 0 应照常启动：%v", name, err)
		}
	}
	// 没有默认值的这几项：不设时就是 0，交给程序里的默认值
	settings := localServerSettings(t)
	if settings.ToolTimeout != 0 || settings.ConfirmationTimeout != 0 || settings.ResearchTimeout != 0 || settings.ResearchMaxRounds != 0 ||
		settings.ResearchMaxSearches != 0 || settings.ResearchMaxTokens != 0 {
		t.Errorf("工具时限、确认时效、调查的上限不设时应是 0（默认值只在 brain / research 里写）：%+v", settings)
	}
}

// 地名库用的语言要是规范写法的小写语言代码（按语言代码表查）。
func TestWeatherGeocodingLanguage(t *testing.T) {
	for _, good := range []string{"ja", "en", "zh", "ko", ""} { // 不设 = 地名库默认的语言
		settings := localServerSettings(t)
		settings.WeatherGeocodingLanguage = good
		if err := settings.validateBrain(); err != nil {
			t.Errorf("%s 应该可以：%v", good, err)
		}
	}
	for _, bad := range []string{"jp", "JA", "jpn", "zh-TW", "日语"} {
		settings := localServerSettings(t)
		settings.WeatherGeocodingLanguage = bad
		if err := settings.validateBrain(); err == nil || !strings.Contains(err.Error(), "HARO_WEATHER_GEOCODING_LANGUAGE") {
			t.Errorf("%q 应起不来并写明 HARO_WEATHER_GEOCODING_LANGUAGE：%v", bad, err)
		}
	}
}

// 启动日志写的时区：缩写、和 UTC 差多少、环境变量 TZ 的值。
func TestLocalTimeZone(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	zone := LocalTimeZone(time.Date(2026, 11, 21, 9, 0, 0, 0, time.FixedZone("JST", 9*3600)))
	if zone != (TimeZone{Name: "JST", Offset: "+09:00", TZ: "Asia/Tokyo"}) {
		t.Errorf("东京：%+v", zone)
	}
	t.Setenv("TZ", "")
	zone = LocalTimeZone(time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC))
	if zone != (TimeZone{Name: "UTC", Offset: "+00:00"}) {
		t.Errorf("UTC、没设 TZ：%+v", zone)
	}
}

// HARO_TOOLS 是内置工具的组：不设（或写成空）= 全开；列出的才开；none = 一个都不要。原来的 HARO_WEATHER=false 起不来、写明怎么改。
func TestToolGroupsFromEnvironment(t *testing.T) {
	for value, want := range map[string][]string{
		"":               ToolGroups,
		"memory,weather": {ToolGroupMemory, ToolGroupWeather},
		"none":           nil,
	} {
		settings := serverSettingsFrom(t, map[string]string{"HARO_TOOLS": value})
		if err := settings.validateToolGroups(); err != nil {
			t.Errorf("HARO_TOOLS=%q 应通过：%v", value, err)
		}
		var on []string
		for _, group := range ToolGroups {
			if settings.ToolGroupOn(group) {
				on = append(on, group)
			}
		}
		if !slices.Equal(on, want) {
			t.Errorf("HARO_TOOLS=%q 开着的组：%v，应是 %v", value, on, want)
		}
	}
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { value, ok := env[name]; return value, ok }
	}
	if _, err := checkRemovedSettings(lookup(map[string]string{"HARO_WEATHER": "false"}), serverSide); err == nil ||
		!strings.Contains(err.Error(), "HARO_TOOLS") {
		t.Errorf("HARO_WEATHER=false 要求的是关天气，应起不来并写明改用 HARO_TOOLS：%v", err)
	}
	if _, err := checkRemovedSettings(lookup(map[string]string{"HARO_WEATHER": "true"}), serverSide); err != nil {
		t.Errorf("HARO_WEATHER=true 只是多余，不该起不来：%v", err)
	}
}
