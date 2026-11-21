package config

import (
	"path/filepath"
	"reflect"
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

// 天气的备用来源只认 jma / none；家的位置写错照样报错。
func TestValidateWeather(t *testing.T) {
	for _, backup := range []string{WeatherBackupJMA, WeatherBackupNone} {
		if err := (ServerSettings{WeatherBackup: backup}).validateWeather(); err != nil {
			t.Errorf("%s 应通过：%v", backup, err)
		}
	}
	if err := (ServerSettings{WeatherBackup: "met"}).validateWeather(); err == nil || !strings.Contains(err.Error(), "HARO_WEATHER_BACKUP") {
		t.Errorf("不认识的备用来源应报错：%v", err)
	}
	if err := (ServerSettings{WeatherBackup: WeatherBackupJMA, WeatherHome: "家"}).validateWeather(); err == nil ||
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
	settings := TerminalSettings{TurnDetection: "server_vad", ServerURL: "http://127.0.0.1:59130", WakeIdle: time.Minute}
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

// 提前压缩的门槛：默认 24000；0 = 不看；负数起不来。
func TestCompactContextTokens(t *testing.T) {
	minimal := map[string]string{"HARO_LLM_PROVIDER": "local", "HARO_TTS": "piper", "HARO_TTS_URL": "http://127.0.0.1:9",
		"HARO_ASR_URL": "http://127.0.0.1:9", "HARO_TOOLS": "false", "HARO_WEATHER": "false", "HARO_SERVER_LISTEN": "127.0.0.1:0",
		"HARO_STRONG_LLM_MODEL": "", "HARO_TAVILY_API_KEY": "", "HARO_CONTEXT_COMPACT_TOKENS": "",
		"HARO_CONVERSATIONS_DB": filepath.Join(t.TempDir(), "conversations.db")}
	for name, value := range minimal {
		t.Setenv(name, value)
	}
	settings, err := LoadBrainSettings()
	if err != nil || settings.CompactContextTokens != 24000 {
		t.Fatalf("默认 24000：%d %v", settings.CompactContextTokens, err)
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
	base := serverSettingsFrom(t, map[string]string{"HARO_TTS": TTSPiper, "HARO_TTS_URL": "http://127.0.0.1:5000",
		"HARO_ASR_URL": "http://127.0.0.1:59126", "HARO_LLM_PROVIDER": LLMLocal, "HARO_TOOLS": "false"})
	if err := base.validateBrain(); err != nil {
		t.Fatalf("基线应通过：%v", err)
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
		{"导入旧对话却没有对话库", func(s *ServerSettings) { s.ConversationsDir, s.ConversationsDB = "/tmp/old", "" }, "HARO_CONVERSATIONS_DB"},
		{"开着工具却没有记事库", func(s *ServerSettings) { s.Tools, s.NotesDB = true, "" }, "HARO_NOTES_DB"},
		{"记事候选上限为零", func(s *ServerSettings) { s.Tools, s.NotesDB, s.NotesListLimit = true, "/tmp/notes.db", 0 }, "HARO_NOTES_LIST_LIMIT"},
		{"压缩门槛是负数", func(s *ServerSettings) { s.CompactContextTokens = -1 }, "HARO_CONTEXT_COMPACT_TOKENS"},
		{"工具时限为零", func(s *ServerSettings) { s.ToolTimeout = 0 }, "HARO_TOOL_TIMEOUT"},
		{"确认时效为零", func(s *ServerSettings) { s.ConfirmationTimeout = 0 }, "HARO_CONFIRMATION_TIMEOUT"},
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
