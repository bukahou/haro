package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/spending"
)

// testAssembly 按 settings 和代理定义（overrides：文件名 → 内容，放进覆盖目录）组装；只建模型、不连网。
func testAssembly(t *testing.T, settings config.ServerSettings, overrides map[string]string) agentAssembly {
	t.Helper()
	dir := ""
	if len(overrides) > 0 {
		dir = t.TempDir()
		for name, text := range overrides {
			if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	catalog, err := agents.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := spending.OpenLedger(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pricing := spending.Pricing{Models: map[string]spending.TokenPrice{"base-model": {Free: true}, "strong-model": {Free: true}}}
	return agentAssembly{settings: settings, catalog: catalog, client: &http.Client{}, pricing: pricing, ledger: ledger,
		guard: spending.NewGuard(ledger, spending.Limits{}, logger), logger: logger}
}

// cloudSettings 是用 OpenAI 兼容接口的云端配置（只建模型、不连网），更强的模型用在 uses 上。
func cloudSettings(uses []string, visionMode string) config.ServerSettings {
	return config.ServerSettings{LLMProvider: config.LLMCloud, CloudLLMAPI: config.CloudAPIOpenAI, CloudLLMURL: "http://127.0.0.1:9/v1",
		CloudLLMModel: "base-model", CloudMaxTokens: 800, StrongLLMModel: "strong-model", StrongLLMUse: uses,
		StrongLLMVisionMode: visionMode, ConversationSummary: true, ExtraSystemPrompt: "追加的一条规矩"}
}

// 程序里带的定义：前台的正文填好了值、接上 HARO_SYSTEM_PROMPT；用在哪由 HARO_STRONG_LLM_USE 定。
func TestBuildAgentsFromBuiltinDefinitions(t *testing.T) {
	built, err := testAssembly(t, cloudSettings([]string{config.StrongForCorrection}, config.VisionByExpert), nil).build(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	front := built.front
	if front.Name != "前台" || front.MaxSteps != 8 || strings.Contains(front.Instructions, "{{") ||
		!strings.HasSuffix(front.Instructions, "\n\n追加的一条规矩") {
		t.Errorf("前台：%+v", front)
	}
	if h := built.handoff; h.Agent == nil || h.Agent.Name != "强模型" || !h.OnCorrection || h.OnFrame || h.Agent.Instructions != "" {
		t.Errorf("只用在纠正上：应能交给强模型接手、画面不交：%+v", h)
	}
	if built.expert != nil {
		t.Error("vision 不在 HARO_STRONG_LLM_USE 里时不应有看画面的专家")
	}
	if built.summary == nil || !strings.Contains(built.summary.Instructions, "摘要") {
		t.Errorf("开了滚动摘要：应有代理「摘要」：%+v", built.summary)
	}
}

// 看画面两种用法：expert = 专家（子任务），takeover = 画面交给强模型接手。没设更强的模型时两样都没有。
func TestBuildAgentsForVisionModes(t *testing.T) {
	expert, err := testAssembly(t, cloudSettings([]string{config.StrongForVision}, config.VisionByExpert), nil).build(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if expert.expert == nil || expert.expert.AgentName() != "看画面" || expert.handoff.Agent != nil {
		t.Errorf("expert：应只有看画面的专家：%+v", expert)
	}
	takeover, err := testAssembly(t, cloudSettings([]string{config.StrongForVision}, config.VisionByTakeover), nil).build(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := takeover.handoff; takeover.expert != nil || h.Agent == nil || !h.OnFrame || h.OnCorrection {
		t.Errorf("takeover：画面应交给强模型接手：%+v", h)
	}
	settings := cloudSettings([]string{config.StrongForVision, config.StrongForCorrection}, config.VisionByExpert)
	settings.StrongLLMModel = ""
	none, err := testAssembly(t, settings, nil).build(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if none.expert != nil || none.handoff.Agent != nil {
		t.Errorf("没设更强的模型：不该有专家和接手：%+v", none)
	}
}

// 覆盖的定义写了认不出的工具、或给摘要写了工具：起不来。阳性对照：同样的覆盖只改对那一处就起得来。
func TestBuildAgentsRejectsBadToolLists(t *testing.T) {
	settings := cloudSettings([]string{config.StrongForCorrection}, config.VisionByExpert)
	front := "---\ntitle: 前台\nmode: primary\nmodel: base\npurpose: chat\nmax_steps: 8\ntools: [%s]\n---\n基础规则。{{.EmotionTags}}\n"
	summary := "---\ntitle: 摘要\nmode: subtask\nmodel: base\npurpose: summary\nmax_steps: 1\ntools: [%s]\n---\n写摘要。\n"
	cases := map[string]struct {
		name, text string
		ok         bool
	}{
		"前台的工具名写错": {agents.Front, strings.Replace(front, "%s", "look_at_camera, ask_stronger_modle", 1), false},
		"前台的工具名都对": {agents.Front, strings.Replace(front, "%s", "look_at_camera, ask_stronger_model", 1), true},
		"摘要写了工具":   {agents.Summary, strings.Replace(summary, "%s", "look_at_camera", 1), false},
		"摘要没写工具":   {agents.Summary, strings.Replace(summary, "%s", "", 1), true},
		"用途写错":     {agents.Summary, strings.Replace(strings.Replace(summary, "%s", "", 1), "purpose: summary", "purpose: summery", 1), false},
	}
	for label, c := range cases {
		_, err := testAssembly(t, settings, map[string]string{c.name: c.text}).build(nil, nil)
		if (err == nil) != c.ok {
			t.Errorf("%s：起得来应为 %v，实际错误 %v", label, c.ok, err)
		}
	}
}

// Claude 特有的取值（回答力度、思考写法）在启动时检查，报错写明是哪个环境变量；用别的接口时不查。
func TestCheckClaudeSettings(t *testing.T) {
	base := config.ServerSettings{CloudLLMAPI: config.CloudAPIAnthropic, CloudLLMEffort: "low", ResearchEffort: "medium",
		StrongLLMEffort: "low", CloudLLMThinking: "disabled", StrongLLMThinking: "between_tools"}
	if err := checkClaudeSettings(base); err != nil {
		t.Fatalf("取值都对应通过：%v", err)
	}
	for name, change := range map[string]func(*config.ServerSettings){
		"HARO_CLOUD_LLM_EFFORT":    func(s *config.ServerSettings) { s.CloudLLMEffort = "max" },
		"HARO_RESEARCH_EFFORT":     func(s *config.ServerSettings) { s.ResearchEffort = "xhigh" },
		"HARO_STRONG_LLM_EFFORT":   func(s *config.ServerSettings) { s.StrongLLMEffort = "max" },
		"HARO_CLOUD_LLM_THINKING":  func(s *config.ServerSettings) { s.CloudLLMThinking = "off" },
		"HARO_STRONG_LLM_THINKING": func(s *config.ServerSettings) { s.StrongLLMThinking = "enabled" },
	} {
		settings := base
		change(&settings)
		if err := checkClaudeSettings(settings); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s 写错：应起不来并写明变量名：%v", name, err)
		}
	}
	openai := base
	openai.CloudLLMAPI, openai.CloudLLMThinking = config.CloudAPIOpenAI, "off"
	if err := checkClaudeSettings(openai); err != nil {
		t.Errorf("用 OpenAI 兼容接口时不查 Claude 的取值：%v", err)
	}
}
