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
