package config

import (
	"strings"
	"testing"
)

// v7：云端用 Claude 时的配置检查（接口格式、模型、凭证、回答力度）。
func TestCloudLLMReadyForAnthropic(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE"} {
		t.Setenv(name, "") // 不受跑测试那台机器的环境影响
	}
	base := Settings{CloudLLMAPI: CloudAPIAnthropic, CloudLLMModel: "claude-haiku-5-5", AnthropicAPIKey: "k",
		CloudLLMEffort: "low", ResearchEffort: "medium"}
	if err := base.cloudLLMReady(); err != nil {
		t.Fatalf("配齐了应通过：%v", err)
	}

	cases := []struct {
		name   string
		change func(*Settings)
		want   string
	}{
		{"没设模型", func(s *Settings) { s.CloudLLMModel = "" }, "HARO_CLOUD_LLM_MODEL"},
		{"没有凭证", func(s *Settings) { s.AnthropicAPIKey = "" }, "HARO_ANTHROPIC_API_KEY"},
		{"力度要开思考才行", func(s *Settings) { s.CloudLLMEffort = "max" }, "low、medium 或 high"},
		{"不认识的接口格式", func(s *Settings) { s.CloudLLMAPI = "gemini" }, "HARO_CLOUD_LLM_API"},
	}
	for _, c := range cases {
		settings := base
		c.change(&settings)
		if err := settings.cloudLLMReady(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应报错且提到 %q，实际 %v", c.name, c.want, err)
		}
	}

	// 集群里：不填 key，靠 Workload Identity Federation（SDK 读 ANTHROPIC_FEDERATION_* 等变量）
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "fdrl_test")
	wif := base
	wif.AnthropicAPIKey = ""
	if err := wif.cloudLLMReady(); err != nil {
		t.Errorf("配了 WIF 时不填 key 也应通过：%v", err)
	}
}

// 天气的备用来源只认 jma / none；家的位置写错照样报错。
func TestValidateWeather(t *testing.T) {
	for _, backup := range []string{WeatherBackupJMA, WeatherBackupNone} {
		if err := (Settings{WeatherBackup: backup}).validateWeather(); err != nil {
			t.Errorf("%s 应通过：%v", backup, err)
		}
	}
	if err := (Settings{WeatherBackup: "met"}).validateWeather(); err == nil || !strings.Contains(err.Error(), "HARO_WEATHER_BACKUP") {
		t.Errorf("不认识的备用来源应报错：%v", err)
	}
	if err := (Settings{WeatherBackup: WeatherBackupJMA, WeatherHome: "家"}).validateWeather(); err == nil ||
		!strings.Contains(err.Error(), "HARO_WEATHER_HOME") {
		t.Errorf("家的位置写错应报错：%v", err)
	}
}

// OpenAI 兼容接口的检查不变：地址、模型、key 都要有。
func TestCloudLLMReadyForOpenAI(t *testing.T) {
	settings := Settings{CloudLLMAPI: CloudAPIOpenAI, CloudLLMURL: "https://example.com/v1", CloudLLMModel: "m", CloudLLMAPIKey: "k"}
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
	for _, name := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE"} {
		t.Setenv(name, "")
	}
	base := Settings{CloudLLMAPI: CloudAPIAnthropic, CloudLLMModel: "claude-haiku-5-5", AnthropicAPIKey: "k",
		CloudLLMEffort: "low", ResearchEffort: "medium", CloudLLMThinking: "disabled", StrongLLMThinking: "between_tools",
		StrongLLMVisionMode: VisionByExpert}
	if err := base.validateStrongLLM(); err != nil {
		t.Fatalf("不设更强的模型应通过：%v", err)
	}
	strong := base
	strong.StrongLLMModel, strong.StrongLLMUse, strong.StrongLLMEffort = "claude-sonnet-5-5", []string{"vision", "correction", "research"}, "low"
	strong.USDToYen, strong.StrongLLMPriceIn, strong.StrongLLMPriceOut = 160, 2, 10
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
		change func(*Settings)
		want   string
	}{
		{"没设单价", func(s *Settings) { s.StrongLLMPriceIn = 0 }, "HARO_STRONG_LLM_PRICE_INPUT"},
		{"不认识的用途", func(s *Settings) { s.StrongLLMUse = []string{"vision", "chat"} }, "HARO_STRONG_LLM_USE"},
		{"力度不对", func(s *Settings) { s.StrongLLMEffort = "max" }, "HARO_STRONG_LLM_EFFORT"},
		{"思考写法不对", func(s *Settings) { s.StrongLLMThinking = "off" }, "HARO_STRONG_LLM_THINKING"},
		{"云端接口没配好", func(s *Settings) { s.AnthropicAPIKey = "" }, "HARO_STRONG_LLM_MODEL"},
		{"看画面的方式不对", func(s *Settings) { s.StrongLLMVisionMode = "both" }, "HARO_STRONG_LLM_VISION_MODE"},
	}
	for _, c := range cases {
		settings := strong
		c.change(&settings)
		if err := settings.validateStrongLLM(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应报错且提到 %q，实际 %v", c.name, c.want, err)
		}
	}
}
