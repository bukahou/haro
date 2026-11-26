//go:build eval

// 搜索质量对比：前台当场查网页这条路，同一批题几种做法各答一遍，看谁答得准（只比搜索引擎和读网页这两层）——
//
//	甲 现在的设置：Tavily 基础档，快查 3 条、读网页每页 2000 字（tools.DefaultWebSearchLimits）
//	乙 Anthropic 自带：服务端的 web_search / web_fetch（服务端自己搜、自己读，再答）；默认由模型直接调用
//	丙 Tavily 调高一档：高级档、偏向日本的结果，快查 5 条、读网页每页 4000 字
//
// 每题每种做法新开一个真的会话（brain.Session）：系统提示词、情况说明（时间、家的城市）、主人资料、默认性格都照线上的来，
// 前台模型、思考、力度、输出上限按服务端的配置（config 的默认值 + 环境变量）；只给查网页的工具
// （乙的服务端工具不经工具箱，系统提示词里「手边的工具」那一行按同样的写法补上）。
//
// 题目不放在这个仓库（有家附近的地名）：HARO_EVAL_DIR 指向题目目录，里面的 cases.yaml 写 id、question、reference、tolerance。
// 分两步，结果写到 HARO_EVAL_DIR/results/：
//  1. TestSearchEvalRun：跑一遍，每次的回答、用时、用量、调了什么边跑边写进 run-<时间>.jsonl（haro 的日志在 run-<时间>.log）；
//  2. 参考答案填进 cases.yaml 后，TestSearchEvalJudge：评审模型（默认 Sonnet）对照参考答案判（看不到是哪种做法），
//     写 report-<时间>.md。评审的判断只是初判，最后以人看为准。
//
// 跑一次要花钱：HARO_EVAL_DRY_RUN=1 只看要回答多少次；HARO_EVAL_BUDGET_USD（默认 3）是一遍的上限，超了就停。
// HARO_EVAL_CASES、HARO_EVAL_ARMS（逗号分隔）只跑其中几题、几种做法；HARO_EVAL_SPEAKER 是声纹认出的人（主人资料按人取）；
// HARO_EVAL_SERVER_FILTERING=1 让乙先用代码筛结果（新版搜索的默认做法；Haiku 5.5 用它时多数把搜索参数写错，所以默认不用）。
//
//	HARO_ANTHROPIC_API_KEY=… HARO_TAVILY_API_KEY=… HARO_CLOUD_LLM_MODEL=… HARO_PROFILE_DIR=… HARO_PERSONAS_DIR=… HARO_WEATHER_HOME=ip \
//	  HARO_EVAL_SPEAKER=… HARO_EVAL_DIR=… go test -tags eval -run SearchEvalRun -v -count=1 -timeout 90m ./internal/llm/
//	HARO_ANTHROPIC_API_KEY=… HARO_EVAL_DIR=… [HARO_EVAL_RUN=…/run-….jsonl] go test -tags eval -run SearchEvalJudge -v -count=1 -timeout 30m ./internal/llm/
package llm

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/caarlos0/env/v11"
	"github.com/google/jsonschema-go/jsonschema"
	"go.yaml.in/yaml/v3"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/persona"
	"github.com/bukahou/haro/internal/pricecatalog"
	"github.com/bukahou/haro/internal/profile"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
	"github.com/bukahou/haro/internal/spending"
	"github.com/bukahou/haro/internal/tools"
	"github.com/bukahou/haro/internal/weather"
)

// 三种做法的名字（报告里也用）。
const (
	armCurrent   = "甲"
	armAnthropic = "乙"
	armTavilyUp  = "丙"
)

var searchArmNotes = map[string]string{
	armCurrent:   "现在的设置：Tavily 基础档，快查 3 条、读网页每页 2000 字",
	armAnthropic: "Anthropic 服务端 web_search_20260318 + web_fetch_20260318（模型直接调用，不用代码筛结果）：每题最多搜 5 次、读 3 页，所在地按家的位置",
	armTavilyUp:  "Tavily 高级档、偏向日本：快查 5 条、读网页每页 4000 字",
}

// 丙的档位与上限。
var (
	tavilyUpOptions = tools.TavilyOptions{Advanced: true, Country: "japan"}
	tavilyUpLimits  = tools.WebSearchLimits{Results: 5, ResultRunes: 5000, PageRunes: 4000}
)

// 乙的设置。官方定价：Claude API 的网页搜索每 1000 次 10 美元，另按 token 计；读网页不另收费；
// 和新版搜索 / 读网页一起用时代码执行（先用代码筛结果）不另收费。
const (
	serverSearchMaxUses = 5
	serverFetchMaxUses  = 3
	// serverFilteringMaxTokens：先用代码筛结果时，写的代码也算输出，线上前台的上限不够用；念出来多长由提示词管
	serverFilteringMaxTokens = 4000
	// serverToolsMaxRounds：服务端搜索的回合到了上限（pause_turn）后最多再接着请求几次
	serverToolsMaxRounds = 4
	webSearchUSD         = 10.0 / 1000
)

// searchAnswerTimeout 是一题从交出识别结果到答完最多等多久。
const searchAnswerTimeout = 3 * time.Minute

// searchCase 是 cases.yaml 里的一道题。
type searchCase struct {
	ID        string   `yaml:"id"`
	Category  string   `yaml:"category"`
	Question  string   `yaml:"question"`
	Timely    bool     `yaml:"timely"`
	Tolerance string   `yaml:"tolerance"`
	Reference string   `yaml:"reference"`
	Sources   []string `yaml:"sources"`
	CheckedAt string   `yaml:"checked_at"`
}

// searchRun 是一次回答（run-<时间>.jsonl 的一行）。
type searchRun struct {
	Case        string    `json:"case"`
	Arm         string    `json:"arm"`
	Question    string    `json:"question"`
	AskedAt     time.Time `json:"asked_at"`
	Answer      string    `json:"answer"`
	Status      string    `json:"status"`           // response.done 的状态
	Reason      string    `json:"reason,omitempty"` // 状态不是 completed 时的原因
	Error       string    `json:"error,omitempty"`  // 评测这边出的错（例如等不到答完）
	FirstTextMs int64     `json:"first_text_ms"`    // 交出识别结果后多久出第一句（0 = 没出）
	DoneMs      int64     `json:"done_ms"`
	Usage       runUsage  `json:"usage"`
}

// failed 是这次没能正常答完（评测出错，或会话报了失败）。
func (r searchRun) failed() bool {
	return r.Error != "" || r.Status != string(realtimeproto.StatusCompleted)
}

// runUsage 是一次回答的用量与调用记录。
type runUsage struct {
	Requests         int      `json:"requests"`
	PromptTokens     int      `json:"prompt_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	CacheReadTokens  int      `json:"cache_read_tokens"`
	CacheWriteTokens int      `json:"cache_write_tokens"`
	ModelUSD         float64  `json:"model_usd"`
	WebSearches      int      `json:"web_searches"`   // 搜索次数（甲 / 丙：调 web_search；乙：服务端搜索）
	PageReads        int      `json:"page_reads"`     // 读网页次数（甲 / 丙：调 read_web_pages；乙：服务端读网页）
	SearchFeeUSD     float64  `json:"search_fee_usd"` // 乙的搜索费
	TavilyCredits    int      `json:"tavily_credits"`
	Trace            []string `json:"trace"` // 依次调了什么（工具名 + 参数）
}

func (u runUsage) totalUSD() float64 { return u.ModelUSD + u.SearchFeeUSD }

// addRequest 加上一次请求的用量；usd 是这次请求按它自己的输入量算的价（超过分档门槛的那次按那一档）。
func (u *runUsage) addRequest(usage brain.ReplyUsage, usd float64) {
	u.Requests++
	u.PromptTokens += usage.PromptTokens
	u.OutputTokens += usage.OutputTokens
	u.CacheReadTokens += usage.CacheReadTokens
	u.CacheWriteTokens += usage.CacheWriteTokens
	u.ModelUSD += usd
}

// usageModel 是记下用量的前台模型。
type usageModel interface {
	brain.ChatModel
	takeUsage() runUsage // 取出到现在为止的用量并清零
}

// searchArm 是一种做法：前台模型、给会话的工具（nil = 不给）、Tavily 积分的计数。
type searchArm struct {
	name  string
	model usageModel
	tools tools.Builtin
	meter *creditMeter
	// serverTools 是模型服务那边提供的工具名（乙）：不经工具箱，系统提示词里按 brain.ToolOverviewOf 的写法介绍
	serverTools []string
}

// searchEnv 是每题都一样的部分：前台的定义、主人资料、性格、每轮的情况说明、价格。
type searchEnv struct {
	settings     config.ServerSettings
	front        agents.Definition
	instructions string
	personas     []brain.Persona
	profiles     brain.ProfileSource
	briefers     []brain.Briefer
	home         weather.Place
	pricing      spending.Pricing // USDToYen = 1：LLMYen 算出来就是美元
	speaker      string
	logger       *slog.Logger
}

func TestSearchEvalRun(t *testing.T) {
	dir := os.Getenv("HARO_EVAL_DIR")
	if dir == "" {
		t.Skip("没设 HARO_EVAL_DIR")
	}
	cases := pickSearchCases(t, loadSearchCases(t, dir))
	armNames := pickSearchArms(t)
	if os.Getenv("HARO_EVAL_DRY_RUN") != "" {
		t.Logf("%d 题 × %d 种做法 = %d 次回答（判分另算同样次数）", len(cases), len(armNames), len(cases)*len(armNames))
		return
	}
	key, tavilyKey := os.Getenv("HARO_ANTHROPIC_API_KEY"), os.Getenv("HARO_TAVILY_API_KEY")
	if key == "" || (tavilyKey == "" && slices.ContainsFunc(armNames, func(name string) bool { return name != armAnthropic })) {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY / HARO_TAVILY_API_KEY")
	}
	var settings config.ServerSettings
	if err := env.Parse(&settings); err != nil {
		t.Fatal(err)
	}
	if settings.CloudLLMModel == "" {
		t.Fatal("要设 HARO_CLOUD_LLM_MODEL（线上前台用的模型）")
	}
	budget := 3.0
	if raw := os.Getenv("HARO_EVAL_BUDGET_USD"); raw != "" {
		var err error
		if budget, err = strconv.ParseFloat(raw, 64); err != nil || budget <= 0 {
			t.Fatalf("HARO_EVAL_BUDGET_USD 要是正数：%q", raw)
		}
	}
	results := filepath.Join(dir, "results")
	if err := os.MkdirAll(results, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Format("20060102-1504")
	logFile, err := os.Create(filepath.Join(results, "run-"+stamp+".log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(logFile, nil))

	ctx := context.Background()
	httpClient := &http.Client{Timeout: 30 * time.Second}
	environment := prepareSearchEnv(t, ctx, settings, httpClient, logger)
	api := claudeAPIForTest(key, "", nil)
	arms := buildSearchArms(t, armNames, environment, api, tavilyKey, logger)

	out, err := os.Create(filepath.Join(results, "run-"+stamp+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	spent := 0.0
	for i, testCase := range cases {
		for _, arm := range rotate(arms, i) { // 每题换个先后，随时间变的题三种做法挨着答
			run := askSearchCase(t, environment, arm, testCase)
			if err := encoder.Encode(run); err != nil {
				t.Fatal(err)
			}
			spent += run.Usage.totalUSD()
			state := "答完"
			if run.failed() {
				state = "失败 " + cmp.Or(run.Error, run.Reason, run.Status)
			}
			t.Logf("%s %s：%s，%.1f 秒，$%.4f（累计 $%.2f），搜 %d 次、读 %d 次：%s", testCase.ID, arm.name, state,
				float64(run.DoneMs)/1000, run.Usage.totalUSD(), spent, run.Usage.WebSearches, run.Usage.PageReads, oneLine(run.Answer, 80))
			if spent > budget {
				t.Fatalf("累计花了 $%.2f，超过这一遍的上限 $%.2f：停下（已答的在 %s）", spent, budget, out.Name())
			}
		}
	}
	t.Logf("跑完：%s，共 $%.2f", out.Name(), spent)
}

// prepareSearchEnv 照服务端的组装方式准备前台（见 cmd/haro-server 的 agents.go、assembly.go、main.go）。
func prepareSearchEnv(t *testing.T, ctx context.Context, settings config.ServerSettings, httpClient *http.Client, logger *slog.Logger) searchEnv {
	t.Helper()
	catalog, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	front, err := catalog.Get(agents.Front)
	if err != nil {
		t.Fatal(err)
	}
	instructions, err := front.RenderPrompt(brain.PromptValues())
	if err != nil {
		t.Fatal(err)
	}
	environment := searchEnv{settings: settings, front: front, instructions: instructions,
		speaker: os.Getenv("HARO_EVAL_SPEAKER"), logger: logger,
		briefers: []brain.Briefer{brain.ClockBriefer{}}}
	if settings.PersonasDir != "" {
		if environment.personas, err = persona.LoadCatalog(settings.PersonasDir); err != nil {
			t.Fatal(err)
		}
	}
	if settings.ProfileDir != "" {
		environment.profiles = profile.OpenDir(settings.ProfileDir, logger)
	}
	homeSetting, err := settings.WeatherHomePlace()
	if err != nil {
		t.Fatal(err)
	}
	if homeSetting.Set {
		var home weather.HomeLocator = weather.FixedHome{Place: weather.Place{Name: settings.WeatherHomeName,
			Latitude: homeSetting.Latitude, Longitude: homeSetting.Longitude}}
		if homeSetting.ByIP {
			home = weather.NewIPHome(settings.WeatherIPLocateURL, settings.WeatherHomeName, httpClient)
		}
		if environment.home, err = home.Home(ctx); err != nil {
			t.Fatalf("定位家的位置：%v", err)
		}
		environment.briefers = append(environment.briefers, brain.BriefLines(weather.LocationNote{Home: home}.Brief))
	}
	prices, _, err := pricecatalog.Load(ctx, httpClient, settings.ModelPricesURL, settings.ModelPricesProvider, "")
	if err != nil {
		t.Fatal(err)
	}
	price, err := prices.Price(settings.CloudLLMModel)
	if err != nil {
		t.Fatal(err)
	}
	environment.pricing = spending.Pricing{USDToYen: 1, Models: map[string]spending.TokenPrice{settings.CloudLLMModel: price}}
	t.Logf("前台：%s（思考 %s、力度 %s、输出上限 %d、最多 %d 步）；性格 %d 个（默认 %s）；主人资料 %t；家 %s %s %s；说话的人 %q",
		settings.CloudLLMModel, settings.CloudLLMThinking, settings.CloudLLMEffort, frontMaxTokens(settings, front), front.MaxSteps,
		len(environment.personas), settings.PersonaDefault, environment.profiles != nil,
		environment.home.Name, environment.home.Region, environment.home.Country, environment.speaker)
	return environment
}

func frontMaxTokens(settings config.ServerSettings, front agents.Definition) int {
	return cmp.Or(front.MaxOutputTokens, settings.CloudMaxTokens)
}

// buildSearchArms 按名字组装做法。前台模型照线上的配置（见 cmd/haro-server 的 meteredAs），
// 只是让 SDK 在限流、服务端一时出错时重试（线上前台不重试，评测不想因为一时出错少一题）。
func buildSearchArms(t *testing.T, names []string, environment searchEnv, api *ClaudeAPI, tavilyKey string, logger *slog.Logger) []searchArm {
	t.Helper()
	settings := environment.settings
	front := func(maxTokens int) *AnthropicChatModel {
		return NewAnthropicChatModel(AnthropicOptions{API: api, Model: settings.CloudLLMModel, MaxTokens: maxTokens,
			Effort: settings.CloudLLMEffort, Thinking: settings.CloudLLMThinking, MaxRetries: 2})
	}
	var arms []searchArm
	for _, name := range names {
		switch name {
		case armCurrent:
			meter := &creditMeter{}
			searcher := tools.NewTavilySearcher(settings.TavilyMCPURL, tavilyKey, nil, logger)
			t.Cleanup(func() { _ = searcher.Close() })
			arms = append(arms, searchArm{name: name, meter: meter, tools: tools.WebSearchTools(searcher, meter, logger),
				model: &meteredFront{ChatModel: front(frontMaxTokens(settings, environment.front)), model: settings.CloudLLMModel,
					pricing: environment.pricing}})
		case armTavilyUp:
			meter := &creditMeter{}
			searcher := tools.NewTavilySearcherWith(settings.TavilyMCPURL, tavilyKey, tavilyUpOptions, nil, logger)
			t.Cleanup(func() { _ = searcher.Close() })
			builtin, err := tools.WebSearchToolsWithLimits(searcher, meter, tavilyUpLimits, logger)
			if err != nil {
				t.Fatal(err)
			}
			arms = append(arms, searchArm{name: name, meter: meter, tools: builtin,
				model: &meteredFront{ChatModel: front(frontMaxTokens(settings, environment.front)), model: settings.CloudLLMModel,
					pricing: environment.pricing}})
		case armAnthropic:
			filtering := os.Getenv("HARO_EVAL_SERVER_FILTERING") != ""
			maxTokens := frontMaxTokens(settings, environment.front)
			if filtering {
				maxTokens = serverFilteringMaxTokens
			}
			serverTools := serverSearchTools(environment.home, filtering)
			var names []string
			for _, tool := range serverTools {
				names = append(names, *tool.GetName())
			}
			arms = append(arms, searchArm{name: name, serverTools: names, model: &serverToolsFront{front: front(maxTokens),
				model: settings.CloudLLMModel, pricing: environment.pricing, tools: serverTools}})
		}
	}
	return arms
}

// serverSearchTools 是乙给模型的服务端工具：搜索带上家所在的城市（让结果按地区来），读网页不另外压字数。
// filtering = 先用代码筛结果（新版的默认做法）；否则由模型直接调用。
func serverSearchTools(home weather.Place, filtering bool) []anthropic.ToolUnionParam {
	var location anthropic.UserLocationParam
	if home.Name != "" {
		location.City = anthropic.String(home.Name)
	}
	if home.Region != "" {
		location.Region = anthropic.String(home.Region)
	}
	if home.Country != "" {
		location.Country = anthropic.String(home.Country)
	}
	search := &anthropic.WebSearchTool20260318Param{MaxUses: anthropic.Int(serverSearchMaxUses), UserLocation: location}
	fetch := &anthropic.WebFetchTool20260318Param{MaxUses: anthropic.Int(serverFetchMaxUses)}
	if !filtering {
		search.AllowedCallers, fetch.AllowedCallers = []string{"direct"}, []string{"direct"}
	}
	return []anthropic.ToolUnionParam{{OfWebSearchTool20260318: search}, {OfWebFetchTool20260318: fetch}}
}

// askSearchCase 新开一个会话，让主人说这道题，收到答完为止。
func askSearchCase(t *testing.T, environment searchEnv, arm searchArm, testCase searchCase) searchRun {
	t.Helper()
	ctx := context.Background()
	front := environment.front
	instructions := environment.instructions
	if overview := brain.ToolOverviewOf(arm.serverTools); overview != "" { // 和工具箱的工具一样，接在基础规则后面（见 composeSystemPrompt）
		instructions += "\n\n" + overview
	}
	agent := brain.Agent{Name: front.Title, Model: arm.model, Instructions: instructions, Tools: front.Tools,
		MaxSteps: front.MaxSteps, RepeatLimit: front.RepeatLimit, RepeatWindow: front.RepeatWindow, Timeout: front.Timeout}
	var toolbox brain.Toolbox // 不给工具时是 nil 接口（不是装着 nil 指针的接口）
	if arm.tools != nil {
		box, err := tools.NewToolbox(ctx, environment.logger, arm.tools)
		if err != nil {
			t.Fatal(err)
		}
		defer box.Close()
		toolbox = box
	}
	settings := environment.settings
	voice := braintest.NewVoice()
	session := brain.NewSession(brain.Components{Streaming: voice, Front: agent, Synthesizer: textAsAudio{}, Tools: toolbox,
		Briefers: environment.briefers, Profiles: environment.profiles,
		NewSegmenter: func() brain.TextSegmenter {
			return speech.NewSentenceSegmenter(settings.FirstSegmentMinRunes, settings.MaxSegmentRunes)
		},
	}, brain.Options{MaxHistoryTurn: 6, Personas: environment.personas, DefaultPersona: settings.PersonaDefault}, environment.logger)
	defer session.Close()

	arm.model.takeUsage() // 清掉上一题剩下的（不该有）
	if arm.meter != nil {
		arm.meter.take()
	}
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{})
	utterance := braintest.Utterance{Text: testCase.Question, Speaker: environment.speaker, SpeakerChecked: environment.speaker != ""}
	braintest.Speak(t, session, stream, utterance)
	braintest.Expect[realtimeproto.ResponseCreated](t, session)
	run := searchRun{Case: testCase.ID, Arm: arm.name, Question: testCase.Question, AskedAt: time.Now()}
	braintest.Recognized(t, stream, utterance)
	answer, done, first, elapsed, err := collectSearchAnswer(session, run.AskedAt, searchAnswerTimeout)
	run.Answer, run.FirstTextMs, run.DoneMs = answer, first.Milliseconds(), elapsed.Milliseconds()
	if err != nil {
		run.Error = err.Error()
	} else {
		run.Status, run.Reason = string(done.Status), string(done.Reason)
	}
	run.Usage = arm.model.takeUsage()
	if arm.meter != nil {
		run.Usage.TavilyCredits = arm.meter.take()
	}
	return run
}

// collectSearchAnswer 收会话的事件直到 response.done（最多等 timeout）：拼出说给主人的文字，记下出第一句和答完的时间。
func collectSearchAnswer(session *brain.Session, asked time.Time, timeout time.Duration) (string, realtimeproto.ResponseDone,
	time.Duration, time.Duration, error) {
	var events []realtimeproto.ServerEvent
	var first time.Duration
	deadline := time.After(timeout)
	for {
		select {
		case event := <-session.Events():
			events = append(events, event)
			switch event := event.(type) {
			case realtimeproto.ResponseOutputAudioTranscriptDelta:
				if first == 0 && event.Delta != "" {
					first = time.Since(asked)
				}
			case realtimeproto.ResponseDone:
				return braintest.Transcript(events), event, first, time.Since(asked), nil
			}
		case <-deadline:
			return braintest.Transcript(events), realtimeproto.ResponseDone{}, first, time.Since(asked),
				fmt.Errorf("等了 %v 还没答完", timeout)
		}
	}
}

// meteredFront 包一层前台模型：记下每次请求的用量（按每次请求自己的输入量算价）和调了哪些工具。
type meteredFront struct {
	brain.ChatModel
	model   string
	pricing spending.Pricing

	mu    sync.Mutex
	usage runUsage
}

func (m *meteredFront) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for delta, err := range m.ChatModel.StreamReply(ctx, request) {
			m.mu.Lock()
			if delta.Usage != nil {
				m.usage.addRequest(*delta.Usage, m.pricing.LLMYen(m.model, *delta.Usage))
			}
			for _, call := range delta.ToolCalls {
				m.usage.Trace = append(m.usage.Trace, traceOf(call.Name, call.Arguments))
				switch call.Name {
				case tools.WebSearchTool:
					m.usage.WebSearches++
				case tools.ReadWebPagesTool:
					m.usage.PageReads++
				}
			}
			m.mu.Unlock()
			if !yield(delta, err) {
				return
			}
		}
	}
}

func (m *meteredFront) takeUsage() runUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	taken := m.usage
	m.usage = runUsage{}
	return taken
}

// serverToolsFront 是乙：请求照线上的前台模型组装（同一份系统提示词、消息、思考与力度，见 AnthropicChatModel.request），
// 只多带 Anthropic 的服务端搜索和读网页；服务端自己搜、自己读，再答，不会有要 haro 执行的工具调用。
// 服务端搜索的回合到了上限（pause_turn）时，把这次的回答原样带回去接着请求。
type serverToolsFront struct {
	front   *AnthropicChatModel
	model   string
	pricing spending.Pricing
	tools   []anthropic.ToolUnionParam

	mu    sync.Mutex
	usage runUsage
}

func (m *serverToolsFront) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		params, err := m.front.request(request)
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		params.Tools = append(params.Tools, m.tools...)
		var total brain.ReplyUsage
		for range serverToolsMaxRounds {
			message, err := m.streamRound(ctx, params, yield)
			if message == nil { // 调用方不要了
				return
			}
			usage := usageOf(message.Usage)
			m.record(*message, *usage)
			total = addUsage(total, *usage)
			if err != nil {
				yield(brain.ReplyDelta{Usage: &total}, fmt.Errorf("请求 Claude: %w", err))
				return
			}
			switch message.StopReason {
			case anthropic.StopReasonPauseTurn:
				params.Messages = append(params.Messages, message.ToParam())
				continue
			case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
				yield(brain.ReplyDelta{Usage: &total, Stop: brain.StopCompleted}, nil)
			case anthropic.StopReasonMaxTokens:
				yield(brain.ReplyDelta{Usage: &total, Stop: brain.StopMaxTokens}, nil)
			case anthropic.StopReasonRefusal:
				yield(brain.ReplyDelta{Usage: &total}, &RefusalError{Category: string(message.StopDetails.Category),
					Explanation: message.StopDetails.Explanation})
			default:
				yield(brain.ReplyDelta{Usage: &total}, fmt.Errorf("Claude 停下的原因是 %s: %w", message.StopReason, brain.ErrStopUnknown))
			}
			return
		}
		yield(brain.ReplyDelta{Usage: &total}, fmt.Errorf("服务端搜索接着请求了 %d 次还没答完", serverToolsMaxRounds))
	}
}

// streamRound 发一次流式请求，边收边交出文字；返回收齐的这次回答（调用方不要了时返回 nil）。
func (m *serverToolsFront) streamRound(ctx context.Context, params anthropic.MessageNewParams,
	yield func(brain.ReplyDelta, error) bool) (*anthropic.Message, error) {
	stream := m.front.api.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var message anthropic.Message
	for stream.Next() {
		event := stream.Current()
		if err := message.Accumulate(event); err != nil {
			return &message, fmt.Errorf("解析 Claude 的流式响应: %w", err)
		}
		if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if text, ok := delta.Delta.AsAny().(anthropic.TextDelta); ok && text.Text != "" {
				if !yield(brain.ReplyDelta{Text: text.Text}, nil) {
					return nil, nil
				}
			}
		}
	}
	return &message, stream.Err()
}

// record 记下一次请求的用量、搜索与读网页次数（服务端报的）和调了什么。
func (m *serverToolsFront) record(message anthropic.Message, usage brain.ReplyUsage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usage.addRequest(usage, m.pricing.LLMYen(m.model, usage))
	searches := int(message.Usage.ServerToolUse.WebSearchRequests)
	m.usage.WebSearches += searches
	m.usage.PageReads += int(message.Usage.ServerToolUse.WebFetchRequests)
	m.usage.SearchFeeUSD += float64(searches) * webSearchUSD
	for _, block := range message.Content {
		switch block := block.AsAny().(type) {
		case anthropic.ServerToolUseBlock:
			m.usage.Trace = append(m.usage.Trace, traceOf(string(block.Name), block.Input))
		case anthropic.WebSearchToolResultBlock: // 结果只记条数或出错的原因（内容是加密的）
			if block.Content.ErrorCode != "" {
				m.usage.Trace = append(m.usage.Trace, "→ 搜索出错："+string(block.Content.ErrorCode))
			} else {
				m.usage.Trace = append(m.usage.Trace, fmt.Sprintf("→ 搜到 %d 条", len(block.Content.OfWebSearchResultBlockArray)))
			}
		case anthropic.WebFetchToolResultBlock:
			if block.Content.ErrorCode != "" {
				m.usage.Trace = append(m.usage.Trace, "→ 读网页出错："+string(block.Content.ErrorCode))
			} else {
				m.usage.Trace = append(m.usage.Trace, "→ 读到 "+block.Content.URL)
			}
		}
	}
}

func (m *serverToolsFront) takeUsage() runUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	taken := m.usage
	m.usage = runUsage{}
	return taken
}

func addUsage(total, usage brain.ReplyUsage) brain.ReplyUsage {
	total.PromptTokens += usage.PromptTokens
	total.OutputTokens += usage.OutputTokens
	total.CacheReadTokens += usage.CacheReadTokens
	total.CacheWriteTokens += usage.CacheWriteTokens
	total.ThinkingChars += usage.ThinkingChars
	return total
}

// creditMeter 是评测用的搜索额度：不拦，只记下用了多少 Tavily 积分。
type creditMeter struct {
	mu      sync.Mutex
	credits int
}

func (m *creditMeter) AllowSearch(context.Context, int) error { return nil }

func (m *creditMeter) RecordSearch(_ context.Context, credits int, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.credits += credits
	return nil
}

func (m *creditMeter) take() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	taken := m.credits
	m.credits = 0
	return taken
}

// textAsAudio 把要念的句子原样当成声音（评测只看文字）。
type textAsAudio struct{}

func (textAsAudio) Synthesize(_ context.Context, utterance brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		yield(realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1}, Data: []byte(utterance.Text)}, nil)
	}
}

// traceOf 是一次调用的记录：工具名 + 参数（JSON，过长截断）。
func traceOf(name string, arguments any) string {
	data, err := json.Marshal(arguments)
	if err != nil {
		return name
	}
	return name + " " + oneLine(string(data), 300)
}

// oneLine 去掉换行、截到 limit 个字。
func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}

// rotate 把 arms 循环左移 by 位（每题换个先后）。
func rotate(arms []searchArm, by int) []searchArm {
	if len(arms) == 0 {
		return arms
	}
	by %= len(arms)
	return append(slices.Clone(arms[by:]), arms[:by]...)
}

func loadSearchCases(t *testing.T, dir string) []searchCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "cases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []searchCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, testCase := range file.Cases {
		if testCase.ID == "" || testCase.Question == "" || seen[testCase.ID] {
			t.Fatalf("cases.yaml：每题要有不重复的 id 和 question：%+v", testCase)
		}
		seen[testCase.ID] = true
	}
	if len(file.Cases) == 0 {
		t.Fatal("cases.yaml 里没有题")
	}
	return file.Cases
}

// pickSearchCases 按 HARO_EVAL_CASES 挑题（不设 = 全部）；写了不存在的编号报错。
func pickSearchCases(t *testing.T, cases []searchCase) []searchCase {
	t.Helper()
	wanted := splitList(os.Getenv("HARO_EVAL_CASES"))
	if len(wanted) == 0 {
		return cases
	}
	var picked []searchCase
	for _, id := range wanted {
		index := slices.IndexFunc(cases, func(testCase searchCase) bool { return testCase.ID == id })
		if index < 0 {
			t.Fatalf("HARO_EVAL_CASES 里的 %s 不在 cases.yaml", id)
		}
		picked = append(picked, cases[index])
	}
	return picked
}

// pickSearchArms 按 HARO_EVAL_ARMS 挑做法（不设 = 甲乙丙）。
func pickSearchArms(t *testing.T) []string {
	t.Helper()
	all := []string{armCurrent, armAnthropic, armTavilyUp}
	wanted := splitList(os.Getenv("HARO_EVAL_ARMS"))
	if len(wanted) == 0 {
		return all
	}
	for _, name := range wanted {
		if !slices.Contains(all, name) {
			t.Fatalf("HARO_EVAL_ARMS 只能是 %s：%q", strings.Join(all, "、"), name)
		}
	}
	return wanted
}

func splitList(raw string) []string {
	var items []string
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// ——— 判分 ———

// 判分的四档（评审经工具交出，不从文字里找词）。
const (
	gradeCorrect  = "correct"
	gradePartial  = "partial"
	gradeNotFound = "not_found"
	gradeWrong    = "wrong"
)

// searchVerdict 是评审交出的判断。
type searchVerdict struct {
	Grade  string `json:"grade" jsonschema:"对照参考答案的判断"`
	Reason string `json:"reason" jsonschema:"一两句理由：对在哪、错在哪"`
	judged bool
	failed bool // 这次回答本身失败了（没判）
}

func (v searchVerdict) mark() string {
	switch {
	case v.failed:
		return "失败"
	case !v.judged:
		return "未判"
	}
	return map[string]string{gradeCorrect: "✓ 对", gradePartial: "◐ 部分对", gradeNotFound: "? 查不到", gradeWrong: "✗ 错"}[v.Grade]
}

// score 是答对分：对 1、部分对 0.5，其余 0。
func (v searchVerdict) score() float64 {
	switch {
	case !v.judged:
		return 0
	case v.Grade == gradeCorrect:
		return 1
	case v.Grade == gradePartial:
		return 0.5
	}
	return 0
}

func TestSearchEvalJudge(t *testing.T) {
	dir := os.Getenv("HARO_EVAL_DIR")
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if dir == "" || key == "" {
		t.Skip("没设 HARO_EVAL_DIR / HARO_ANTHROPIC_API_KEY")
	}
	cases := map[string]searchCase{}
	for _, testCase := range loadSearchCases(t, dir) {
		cases[testCase.ID] = testCase
	}
	runPath := os.Getenv("HARO_EVAL_RUN")
	if runPath == "" {
		runPath = newestSearchRun(t, filepath.Join(dir, "results"))
	}
	runs := loadSearchRuns(t, runPath)
	var missing []string
	for _, run := range runs {
		if strings.TrimSpace(cases[run.Case].Reference) == "" && !slices.Contains(missing, run.Case) {
			missing = append(missing, run.Case)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("这些题还没填参考答案：%s", strings.Join(missing, "、"))
	}
	var settings config.ServerSettings
	if err := env.Parse(&settings); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	judgeModel := cmp.Or(os.Getenv("HARO_EVAL_JUDGE_MODEL"), "claude-sonnet-5-5")
	prices, _, err := pricecatalog.Load(ctx, &http.Client{Timeout: 30 * time.Second}, settings.ModelPricesURL, settings.ModelPricesProvider, "")
	if err != nil {
		t.Fatal(err)
	}
	price, err := prices.Price(judgeModel)
	if err != nil {
		t.Fatal(err)
	}
	pricing := spending.Pricing{USDToYen: 1, Models: map[string]spending.TokenPrice{judgeModel: price}}
	judge := &meteredFront{ChatModel: NewAnthropicChatModel(AnthropicOptions{API: claudeAPIForTest(key, "", nil), Model: judgeModel,
		MaxTokens: 1000, Effort: "low", Thinking: ThinkingBetweenTools, MaxRetries: 2}), model: judgeModel, pricing: pricing}
	verdicts := make([]searchVerdict, len(runs))
	for i, run := range runs {
		if run.failed() {
			verdicts[i] = searchVerdict{failed: true}
			continue
		}
		verdicts[i] = judgeSearchAnswer(t, judge, cases[run.Case], run)
		t.Logf("%s %s：%s %s", run.Case, run.Arm, verdicts[i].mark(), verdicts[i].Reason)
	}
	judgeUsage := judge.takeUsage()
	report := searchEvalReport(cases, runs, verdicts, filepath.Base(runPath), judgeModel, judgeUsage.ModelUSD)
	path := filepath.Join(dir, "results", "report-"+time.Now().Format("20060102-1504")+".md")
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("报告写到 %s（判分花了 $%.3f）", path, judgeUsage.ModelUSD)
}

// judgeSearchAnswer 请评审对照参考答案判一次回答（看不到是哪种做法），经 submit_grade 交出判断。
func judgeSearchAnswer(t *testing.T, judge brain.ChatModel, testCase searchCase, run searchRun) searchVerdict {
	t.Helper()
	schema, err := jsonschema.For[searchVerdict](nil)
	if err != nil {
		t.Fatal(err)
	}
	schema.Properties["grade"].Enum = []any{gradeCorrect, gradePartial, gradeNotFound, gradeWrong}
	parameters, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	tool := brain.ToolSpec{Name: "submit_grade", Parameters: parameters,
		Description: "交出判断。correct：关键信息和参考答案一致（在判分标准的误差范围内）；partial：方向对，但缺了关键的一部分或有小错；" +
			"not_found：明确说没查到、不确定或查不了，没有给出具体的错误信息；wrong：关键信息和参考答案不符、编造，或者把别的东西当成了问的东西。" +
			"回答是说给人听的，不看文笔和长短，只看信息对不对。"}
	question := fmt.Sprintf("问题：%s\n提问时间：%s（日本时间）\n参考答案：%s\n判分标准：%s\n回答：%s", testCase.Question,
		run.AskedAt.In(japanTime()).Format("2006-01-02 15:04"), testCase.Reference, testCase.Tolerance, run.Answer)
	messages := []brain.ChatMessage{
		{Role: brain.RoleSystem, Text: "你是评测的评审：对照参考答案，判断家用语音助手对这个问题的回答。只通过 submit_grade 工具交出判断。"},
		{Role: brain.RoleUser, Text: question}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for delta, err := range judge.StreamReply(ctx, brain.ChatRequest{Messages: messages, Tools: []brain.ToolSpec{tool}}) {
		if err != nil {
			t.Logf("评审出错：%v", err)
			return searchVerdict{}
		}
		for _, call := range delta.ToolCalls {
			if call.Name != tool.Name {
				continue
			}
			raw, _ := json.Marshal(call.Arguments)
			var verdict searchVerdict
			if err := json.Unmarshal(raw, &verdict); err == nil && slices.Contains([]string{gradeCorrect, gradePartial, gradeNotFound, gradeWrong}, verdict.Grade) {
				verdict.judged = true
				return verdict
			}
		}
	}
	return searchVerdict{}
}

func japanTime() *time.Location {
	location, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		return time.FixedZone("JST", 9*60*60)
	}
	return location
}

func newestSearchRun(t *testing.T, results string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(results, "run-*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("%s 里没有 run-*.jsonl：%v", results, err)
	}
	slices.Sort(matches)
	return matches[len(matches)-1]
}

func loadSearchRuns(t *testing.T, path string) []searchRun {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var runs []searchRun
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var run searchRun
		if err := json.Unmarshal(scanner.Bytes(), &run); err != nil {
			t.Fatalf("%s：%v", path, err)
		}
		runs = append(runs, run)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatalf("%s 里没有回答", path)
	}
	return runs
}

// armTotals 是一种做法的汇总。
type armTotals struct {
	answers                                   int
	score                                     float64
	correct, partial, notFound, wrong, failed int
	firstMs, doneMs                           int64
	firstCount                                int
	usd                                       float64
	searches, reads, credits                  int
}

// searchEvalReport 是 Markdown 报告：汇总、按事先定的标准比较、逐题一览、明细（评审的判断只是初判）。
func searchEvalReport(cases map[string]searchCase, runs []searchRun, verdicts []searchVerdict, runName, judgeModel string, judgeUSD float64) string {
	var report strings.Builder
	fmt.Fprintf(&report, "# 搜索质量对比 %s\n\n回答：`%s`；判分：%s（初判，最后以人看为准；判分花了 $%.3f）\n\n",
		time.Now().Format("2006-01-02 15:04"), runName, judgeModel, judgeUSD)
	var armOrder []string
	for _, name := range []string{armCurrent, armAnthropic, armTavilyUp} {
		if slices.ContainsFunc(runs, func(run searchRun) bool { return run.Arm == name }) {
			armOrder = append(armOrder, name)
		}
	}
	totals := map[string]*armTotals{}
	for _, name := range armOrder {
		totals[name] = &armTotals{}
	}
	for i, run := range runs {
		sum, verdict := totals[run.Arm], verdicts[i]
		sum.answers++
		sum.score += verdict.score()
		switch {
		case verdict.failed:
			sum.failed++
		case !verdict.judged:
		case verdict.Grade == gradeCorrect:
			sum.correct++
		case verdict.Grade == gradePartial:
			sum.partial++
		case verdict.Grade == gradeNotFound:
			sum.notFound++
		case verdict.Grade == gradeWrong:
			sum.wrong++
		}
		if run.FirstTextMs > 0 {
			sum.firstMs += run.FirstTextMs
			sum.firstCount++
		}
		sum.doneMs += run.DoneMs
		sum.usd += run.Usage.totalUSD()
		sum.searches += run.Usage.WebSearches
		sum.reads += run.Usage.PageReads
		sum.credits += run.Usage.TavilyCredits
	}
	report.WriteString("## 汇总\n\n| 做法 | 答对分 | 对 | 部分对 | 查不到 | 错 | 失败 | 第一句（平均） | 答完（平均） | 搜索 / 读网页 | Tavily 积分 | 每题花费 | 合计 |\n" +
		"|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, name := range armOrder {
		sum := totals[name]
		first := "—"
		if sum.firstCount > 0 {
			first = fmt.Sprintf("%.1f 秒", float64(sum.firstMs)/float64(sum.firstCount)/1000)
		}
		fmt.Fprintf(&report, "| %s | %.1f / %d | %d | %d | %d | %d | %d | %s | %.1f 秒 | %d / %d | %d | $%.4f | $%.3f |\n", name, sum.score, sum.answers,
			sum.correct, sum.partial, sum.notFound, sum.wrong, sum.failed, first, float64(sum.doneMs)/float64(max(sum.answers, 1))/1000,
			sum.searches, sum.reads, sum.credits, sum.usd/float64(max(sum.answers, 1)), sum.usd)
	}
	report.WriteString("\n")
	for _, name := range armOrder {
		fmt.Fprintf(&report, "- %s：%s\n", name, searchArmNotes[name])
	}
	if base, ok := totals[armCurrent]; ok {
		report.WriteString("\n事先定的标准：答对分比甲多 4 分以上算明显（部分对算半题），差 1–3 分算打平；答错单独比。\n\n")
		for _, name := range armOrder {
			if name == armCurrent {
				continue
			}
			sum := totals[name]
			fmt.Fprintf(&report, "- %s 比甲：答对分 %+.1f，答错 %+d，失败 %+d\n", name, sum.score-base.score, sum.wrong-base.wrong, sum.failed-base.failed)
		}
	}

	var caseOrder []string
	for _, run := range runs {
		if !slices.Contains(caseOrder, run.Case) {
			caseOrder = append(caseOrder, run.Case)
		}
	}
	slices.Sort(caseOrder)
	report.WriteString("\n## 逐题\n\n| 题 | " + strings.Join(armOrder, " | ") + " |\n|---|" + strings.Repeat("---|", len(armOrder)) + "\n")
	for _, id := range caseOrder {
		row := []string{id + " " + oneLine(cases[id].Question, 24)}
		for _, name := range armOrder {
			cell := "—"
			for i, run := range runs {
				if run.Case == id && run.Arm == name {
					cell = verdicts[i].mark()
				}
			}
			row = append(row, cell)
		}
		report.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}

	report.WriteString("\n## 明细\n")
	for _, id := range caseOrder {
		testCase := cases[id]
		fmt.Fprintf(&report, "\n### %s %s\n\n参考答案：%s\n\n判分标准：%s\n\n", id, testCase.Question, oneLine(testCase.Reference, 600), testCase.Tolerance)
		for _, name := range armOrder {
			for i, run := range runs {
				if run.Case != id || run.Arm != name {
					continue
				}
				verdict := verdicts[i]
				fmt.Fprintf(&report, "- **%s**（%s 问）%s %s\n  - 回答：%s\n", name, run.AskedAt.In(japanTime()).Format("15:04:05"),
					verdict.mark(), verdict.Reason, cmp.Or(oneLine(run.Answer, 800), "（没说话）"))
				if run.failed() {
					fmt.Fprintf(&report, "  - 失败：%s\n", cmp.Or(run.Error, run.Reason, run.Status))
				}
				fmt.Fprintf(&report, "  - 第一句 %.1f 秒、答完 %.1f 秒；请求模型 %d 次，输入 %d / 输出 %d token；$%.4f；搜索 %d 次、读网页 %d 次、Tavily %d 积分\n",
					float64(run.FirstTextMs)/1000, float64(run.DoneMs)/1000, run.Usage.Requests, run.Usage.PromptTokens, run.Usage.OutputTokens,
					run.Usage.totalUSD(), run.Usage.WebSearches, run.Usage.PageReads, run.Usage.TavilyCredits)
				for _, trace := range run.Usage.Trace {
					fmt.Fprintf(&report, "  - 调用：%s\n", trace)
				}
			}
		}
	}
	return report.String()
}
