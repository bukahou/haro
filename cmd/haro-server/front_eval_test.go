//go:build eval

// 前台能力对比：线上的前台（全套工具、系统提示词、主人资料、默认性格、情况说明）答同一批题，比较前台用 Claude
// （HARO_CLOUD_LLM_MODEL，思考、力度、输出上限按服务端配置）和本机 OpenAI 兼容服务上的模型（HARO_EVAL_LOCAL_URL，例如 llama.cpp）。
//
// 工具照 buildTooling 的顺序装配（记忆、提醒、天气、联网搜索、后台调查），开始前核对工具清单和服务端的装配（fullTooling）一致。
// 提醒与记忆每题用新建的临时库（不碰线上）；天气、联网搜索真的查；后台调查换成只记下「派了调查」的假桌子（不真跑，也不花调查的钱）；
// 主人纠正时接手的强模型换成固定回答（只看前台该不该交出去）。每题的第一句照线上是叫醒后说的（开头带着唤醒词）。
//
// 题目不放在这个仓库（有家里的信息）：HARO_EVAL_DIR 指向题目目录（cases.yaml），结果写到 results/：
//  1. TestFrontEvalRun：边跑边写 run-<时间>.jsonl（每轮的回答与用时、工具调用与结果摘要、最后提醒库 / 记忆库里有什么、每次请求的输入 token）；
//  2. TestFrontEvalJudge：评审模型（默认 Sonnet）对照参考答案判（看不到是哪一组），写 report-<时间>.md。评审的判断只是初判。
//
// 组（HARO_EVAL_GROUPS，逗号分隔，默认全部）：haiku（Claude）、local（本机，请求里关掉思考）、local-think（本机，思考开着，
// 输出上限 HARO_EVAL_THINK_MAX_TOKENS，默认 2000）。本机两组的压缩门槛 HARO_EVAL_LOCAL_COMPACT（默认 12000：本机上下文 16k）。
// HARO_EVAL_CASES 挑题；HARO_EVAL_BUDGET_USD（默认 1）超了就停；HARO_EVAL_SPEAKER 是声纹认出的人（主人资料、提醒、记忆按人）。
//
//	HARO_ANTHROPIC_API_KEY=… HARO_TAVILY_API_KEY=… HARO_CLOUD_LLM_MODEL=… HARO_PROFILE_DIR=… HARO_PERSONAS_DIR=… HARO_WEATHER_HOME=ip \
//	  HARO_EVAL_SPEAKER=… HARO_EVAL_LOCAL_URL=http://127.0.0.1:8081/v1 HARO_EVAL_DIR=… \
//	  go test -tags eval -run FrontEvalRun -v -count=1 -timeout 120m ./cmd/haro-server/
//	HARO_ANTHROPIC_API_KEY=… HARO_EVAL_DIR=… [HARO_EVAL_RUN=…/run-….jsonl] go test -tags eval -run FrontEvalJudge -v -count=1 -timeout 60m ./cmd/haro-server/
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"go.yaml.in/yaml/v3"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/memory"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/persona"
	"github.com/bukahou/haro/internal/pricecatalog"
	"github.com/bukahou/haro/internal/profile"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/reminders"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/speech"
	"github.com/bukahou/haro/internal/spending"
	"github.com/bukahou/haro/internal/tools"
	"github.com/bukahou/haro/internal/weather"
)

// 组的名字（报告里也用）。
const (
	groupHaiku      = "haiku"
	groupLocal      = "local"
	groupLocalThink = "local-think"
)

var frontGroupLabels = map[string]string{groupHaiku: "Haiku 5.5", groupLocal: "本机·思考关", groupLocalThink: "本机·思考开"}

// strongAnswer 是假的强模型的固定回答：只有该交出去的那题（日本最高的山）对得上，别的题交出去会露馅。
const strongAnswer = "日本最高的山是富士山，海拔 3776 米。"

// frontWakePhrase 是叫醒用的唤醒词，和 haro-asr 关键词文件里现在的唤醒词一致：识别流报的唤醒词是它，
// 叫醒后的第一句也原样带着它（识别流不删字）。
const frontWakePhrase = "哈罗哈罗"

// frontTurnTimeout 是一轮从交出识别结果到答完最多等多久（本机思考开着时慢）。
const frontTurnTimeout = 4 * time.Minute

// frontCase 是 cases.yaml 里的一道题。
type frontCase struct {
	ID           string   `yaml:"id"`
	Category     string   `yaml:"category"`
	Turns        []string `yaml:"turns"`
	SeedMemories []string `yaml:"seed_memories"`
	Expect       string   `yaml:"expect"`
	ExpectTools  []string `yaml:"expect_tools"`
}

// frontTurn 是一轮：主人说的、什么时候说的、Haro 答的、用时。
type frontTurn struct {
	Said    string    `json:"said"`
	AskedAt time.Time `json:"asked_at"`
	Answer  string    `json:"answer"`
	Status  string    `json:"status"`
	Reason  string    `json:"reason,omitempty"`
	Error   string    `json:"error,omitempty"`
	FirstMs int64     `json:"first_ms"`
	DoneMs  int64     `json:"done_ms"`
}

// frontRun 是一组答一道题（run-<时间>.jsonl 的一行）。
type frontRun struct {
	Case      string      `json:"case"`
	Group     string      `json:"group"`
	Category  string      `json:"category"`
	Turns     []frontTurn `json:"turns"`
	Trace     []string    `json:"trace"`     // 依次调了什么工具、结果摘要
	Tools     []string    `json:"tools"`     // 调过的工具名
	Missing   []string    `json:"missing"`   // expect_tools 里没调的
	Reminders []string    `json:"reminders"` // 结束时在用的提醒
	Memories  []string    `json:"memories"`  // 结束时的记忆
	Offered   int         `json:"offered"`   // 第一次请求给模型的工具数（核对和线上一样）
	Prompts   []int       `json:"prompts"`   // 每次请求的输入 token
	Output    int         `json:"output"`
	Thinking  int         `json:"thinking"` // 思考内容的字数
	USD       float64     `json:"usd"`
}

// failed：有一轮没正常答完。
func (r frontRun) failed() bool {
	for _, turn := range r.Turns {
		if turn.Error != "" || turn.Status != string(realtimeproto.StatusCompleted) {
			return true
		}
	}
	return false
}

// frontShared 是每题都一样的部分。
type frontShared struct {
	settings     config.ServerSettings
	front        agents.Definition
	instructions string
	personas     []brain.Persona
	profiles     brain.ProfileSource
	home         weather.HomeLocator
	openMeteo    *weather.OpenMeteoClient
	forecaster   weather.Forecaster
	searcher     webSearchService
	pricing      spending.Pricing
	api          *llm.ClaudeAPI
	localURL     string
	speaker      string
	logger       *slog.Logger
}

func TestFrontEvalRun(t *testing.T) {
	dir := os.Getenv("HARO_EVAL_DIR")
	if dir == "" {
		t.Skip("没设 HARO_EVAL_DIR")
	}
	cases := pickFrontCases(t, loadFrontCases(t, dir))
	groups := pickFrontGroups(t)
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if key == "" || os.Getenv("HARO_TAVILY_API_KEY") == "" {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY / HARO_TAVILY_API_KEY")
	}
	var settings config.ServerSettings
	if err := env.Parse(&settings); err != nil {
		t.Fatal(err)
	}
	settings.ToolGroups = nil // 内置工具全开
	if settings.CloudLLMModel == "" {
		t.Fatal("要设 HARO_CLOUD_LLM_MODEL（线上前台用的模型）")
	}
	budget := envFloat(t, "HARO_EVAL_BUDGET_USD", 1)
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
	shared := prepareFrontShared(t, ctx, settings, key, logger)
	checkFrontToolsMatchServer(t, ctx, shared)

	out, err := os.Create(filepath.Join(results, "run-"+stamp+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	spent := 0.0
	for i, testCase := range cases {
		for j := range groups {
			group := groups[(i+j)%len(groups)] // 每题换个先后
			run := runFrontCase(t, ctx, shared, group, testCase)
			if err := encoder.Encode(run); err != nil {
				t.Fatal(err)
			}
			spent += run.USD
			last := run.Turns[len(run.Turns)-1]
			state := "答完"
			if run.failed() {
				state = "失败"
			}
			t.Logf("%s %s：%s，最后一轮 %.1f 秒，给了 %d 个工具，输入最多 %d token，$%.4f（累计 $%.2f），调了 %v：%s", testCase.ID,
				frontGroupLabels[group], state, float64(last.DoneMs)/1000, run.Offered, maxInt(run.Prompts), run.USD, spent, run.Tools,
				evalOneLine(last.Answer, 70))
			if spent > budget {
				t.Fatalf("累计 $%.2f 超过上限 $%.2f：停下（已答的在 %s）", spent, budget, out.Name())
			}
		}
	}
	t.Logf("跑完：%s，共 $%.2f", out.Name(), spent)
}

// prepareFrontShared 照服务端的组装方式准备每题都一样的部分（见 agents.go、assembly.go、main.go）。
func prepareFrontShared(t *testing.T, ctx context.Context, settings config.ServerSettings, key string, logger *slog.Logger) frontShared {
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
	shared := frontShared{settings: settings, front: front, instructions: instructions, logger: logger,
		localURL: os.Getenv("HARO_EVAL_LOCAL_URL"), speaker: os.Getenv("HARO_EVAL_SPEAKER")}
	if settings.PersonasDir != "" {
		if shared.personas, err = persona.LoadCatalog(settings.PersonasDir); err != nil {
			t.Fatal(err)
		}
	}
	if settings.ProfileDir != "" {
		shared.profiles = profile.OpenDir(settings.ProfileDir, logger)
	}
	weatherHTTP := weather.NewHTTPClient()
	shared.home = weatherHome(ctx, settings, weatherHTTP, logger)
	shared.openMeteo = weather.NewOpenMeteoClient(settings.WeatherForecastURL, settings.WeatherGeocodingURL, weatherHTTP)
	shared.forecaster = weatherForecaster(ctx, settings, shared.openMeteo, shared.home, weatherHTTP, logger)
	shared.searcher = connectWebSearch(settings, &http.Client{}, logger)
	if shared.searcher == nil {
		t.Fatal("联网搜索没开（HARO_TAVILY_API_KEY）")
	}
	t.Cleanup(func() { _ = shared.searcher.Close() })
	if shared.api, err = llm.NewClaudeAPI(key, "", nil); err != nil {
		t.Fatal(err)
	}
	prices, _, err := pricecatalog.Load(ctx, &http.Client{Timeout: 30 * time.Second}, settings.ModelPricesURL, settings.ModelPricesProvider, "")
	if err != nil {
		t.Fatal(err)
	}
	price, err := prices.Price(settings.CloudLLMModel)
	if err != nil {
		t.Fatal(err)
	}
	shared.pricing = spending.Pricing{Models: map[string]spending.TokenPrice{settings.CloudLLMModel: price, "local": {Free: true}}}
	t.Logf("前台（Claude 组）：%s（思考 %s、力度 %s、输出上限 %d、最多 %d 步）；本机：%q；性格 %d 个（默认 %s）；主人资料 %t；说话的人 %q",
		settings.CloudLLMModel, settings.CloudLLMThinking, settings.CloudLLMEffort, frontOutputLimit(settings, front), front.MaxSteps,
		shared.localURL, len(shared.personas), settings.PersonaDefault, shared.profiles != nil, shared.speaker)
	return shared
}

func frontOutputLimit(settings config.ServerSettings, front agents.Definition) int {
	return cmp.Or(front.MaxOutputTokens, settings.CloudMaxTokens)
}

// frontCaseKit 是一题用的东西：临时库、工具箱、调度。
type frontCaseKit struct {
	notes     notes.Repository
	reminders *reminders.SQLiteStore
	keeper    *memory.Keeper
	scheduler *reminders.Scheduler
	notices   *brain.NoticeHub
	toolbox   *tools.Toolbox
	stop      func()
}

// newFrontCaseKit 照 buildTooling 的顺序装配工具箱，只把后台调查换成 desk（假的）。
func newFrontCaseKit(t *testing.T, ctx context.Context, shared frontShared, desk tools.ResearchDesk) frontCaseKit {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.db")
	store, err := notes.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	reminderStore, err := reminders.OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	notices := brain.NewNoticeHub()
	keeper := memory.NewKeeper(store, nil, nil, shared.settings.NotesListLimit, shared.logger)
	scheduler := reminders.NewScheduler(reminderStore, notices, reminders.SchedulerOptions{}, shared.logger)
	runCtx, cancel := context.WithCancel(ctx)
	running := make(chan struct{})
	go func() {
		defer close(running)
		scheduler.Run(runCtx)
	}()
	meter := &frontSearchMeter{}
	builtins := []tools.Builtin{tools.MemoryTools(keeper, time.Local, shared.logger), tools.ReminderTools(scheduler, shared.logger),
		tools.WeatherTools(shared.openMeteo, shared.forecaster, shared.home, shared.settings.WeatherGeocodingLanguage, shared.logger),
		tools.WebSearchTools(shared.searcher, meter, shared.logger), tools.ResearchTools(desk)}
	toolbox, err := tools.NewToolbox(ctx, shared.logger, builtins...)
	if err != nil {
		t.Fatal(err)
	}
	return frontCaseKit{notes: store, reminders: reminderStore, keeper: keeper, scheduler: scheduler, notices: notices, toolbox: toolbox,
		stop: func() {
			toolbox.Close()
			cancel()
			<-running
			_ = reminderStore.Close()
			if closer, ok := store.(io.Closer); ok {
				_ = closer.Close()
			}
		}}
}

// checkFrontToolsMatchServer 核对评测装配的工具清单和服务端（fullTooling）一样：装配漂了就不比（阳性对照：清单不能为空）。
func checkFrontToolsMatchServer(t *testing.T, ctx context.Context, shared frontShared) {
	t.Helper()
	kit := newFrontCaseKit(t, ctx, shared, &recordedDesk{})
	defer kit.stop()
	names := func(specs []brain.ToolSpec) []string {
		var list []string
		for _, spec := range specs {
			list = append(list, spec.Name)
		}
		slices.Sort(list)
		return list
	}
	ours, server := names(kit.toolbox.Specs()), names(fullTooling(t).toolbox.Specs())
	if len(ours) == 0 || !slices.Equal(ours, server) {
		t.Fatalf("评测装配的工具和服务端的不一样：\n评测 %v\n服务端 %v", ours, server)
	}
	t.Logf("工具箱和服务端一致：%d 个（%s）", len(ours), strings.Join(ours, "、"))
}

// frontModel 按组建前台模型。
func frontModel(t *testing.T, shared frontShared, group string) (brain.ChatModel, string, int) {
	t.Helper()
	settings := shared.settings
	limit := frontOutputLimit(settings, shared.front)
	switch group {
	case groupHaiku:
		return llm.NewAnthropicChatModel(llm.AnthropicOptions{API: shared.api, Model: settings.CloudLLMModel, MaxTokens: limit,
			Effort: settings.CloudLLMEffort, Thinking: settings.CloudLLMThinking, MaxRetries: 2}), settings.CloudLLMModel, settings.CompactContextTokens
	case groupLocal, groupLocalThink:
		if shared.localURL == "" {
			t.Fatal("本机那两组要设 HARO_EVAL_LOCAL_URL")
		}
		client := &http.Client{}
		if group == groupLocal {
			client.Transport = noThinkTransport{base: http.DefaultTransport}
		} else {
			limit = int(envFloat(t, "HARO_EVAL_THINK_MAX_TOKENS", 2000))
		}
		return llm.NewOpenAIChatModel(llm.OpenAIOptions{BaseURL: shared.localURL, Model: "local", MaxTokens: limit, MaxRetries: 2,
			HTTPClient: client}), "local", int(envFloat(t, "HARO_EVAL_LOCAL_COMPACT", 12000))
	}
	t.Fatalf("不认识的组 %q", group)
	return nil, "", 0
}

// runFrontCase 新建临时库和会话，让主人依次说这道题的几句话，收到每轮答完为止；最后记下提醒库、记忆库里有什么。
func runFrontCase(t *testing.T, ctx context.Context, shared frontShared, group string, testCase frontCase) frontRun {
	t.Helper()
	desk := &recordedDesk{}
	kit := newFrontCaseKit(t, ctx, shared, desk)
	defer kit.stop()
	for _, content := range testCase.SeedMemories {
		if _, err := kit.notes.Save(ctx, notes.NewNote{Content: content, Source: notes.SourceOwner}); err != nil {
			t.Fatal(err)
		}
	}
	model, modelID, compact := frontModel(t, shared, group)
	recorder := &frontRecorder{ChatModel: model, model: modelID, pricing: shared.pricing, seen: map[string]bool{}}
	strong := brain.Agent{Name: "更强的模型", Model: fixedAnswerModel{text: strongAnswer}}
	front := shared.front
	settings := shared.settings
	voice := braintest.NewVoice()
	session := brain.NewSession(brain.Components{Streaming: voice,
		Front: brain.Agent{Name: front.Title, Model: recorder, Instructions: shared.instructions, Tools: front.Tools,
			MaxSteps: front.MaxSteps, RepeatLimit: front.RepeatLimit, RepeatWindow: front.RepeatWindow, Timeout: front.Timeout},
		Handoff:     brain.Handoff{Agent: &strong, OnCorrection: true},
		Synthesizer: frontTextAudio{}, Tools: kit.toolbox, Notices: kit.notices, Profiles: shared.profiles,
		Briefers: frontBriefers(shared, kit), Camera: braintest.NewCamera(nil),
		NewSegmenter: func() brain.TextSegmenter {
			return speech.NewSentenceSegmenter(settings.FirstSegmentMinRunes, settings.MaxSegmentRunes)
		},
	}, brain.Options{MaxHistoryTurn: settings.MaxHistoryTurn, CompactContextTokens: compact, Personas: shared.personas,
		DefaultPersona: settings.PersonaDefault}, shared.logger)
	defer session.Close()
	// 线上终端用唤醒门：每题的第一句是叫醒后说的，后面几句是醒着时接着说的
	stream := braintest.Open(t, session, voice, realtimeproto.SessionUpdate{WakeWord: true})
	run := frontRun{Case: testCase.ID, Group: group, Category: testCase.Category}
	for i, said := range testCase.Turns {
		heard := said
		if i == 0 {
			// 照线上叫醒：识别流先报听到的唤醒词，再把整句（开头带着唤醒词）交过来。
			// braintest.Speak 报的是占位的唤醒词，这里自己报，Speak 只报开始说 / 说完了
			heard = frontWakePhrase + "，" + said
			if err := stream.Emit(realtimeproto.HaroWakeWordDetected{Keyword: frontWakePhrase}); err != nil {
				t.Fatal(err)
			}
			braintest.Expect[realtimeproto.HaroWakeWordDetected](t, session)
		}
		utterance := braintest.Utterance{Text: heard, Speaker: shared.speaker, SpeakerChecked: shared.speaker != ""}
		braintest.Speak(t, session, stream, utterance)
		braintest.Expect[realtimeproto.ResponseCreated](t, session)
		turn := frontTurn{Said: heard, AskedAt: time.Now()}
		utterance.Wake = i == 0
		braintest.Recognized(t, stream, utterance)
		answer, done, first, elapsed, err := collectFrontAnswer(session, turn.AskedAt, frontTurnTimeout)
		turn.Answer, turn.FirstMs, turn.DoneMs = answer, first.Milliseconds(), elapsed.Milliseconds()
		if err != nil {
			turn.Error = err.Error()
		} else {
			turn.Status, turn.Reason = string(done.Status), string(done.Reason)
		}
		run.Turns = append(run.Turns, turn)
		if err != nil {
			break
		}
	}
	recorder.fill(&run)
	for _, question := range desk.questions() {
		run.Trace = append(run.Trace, "（假的后台调查桌记下：派了调查「"+question+"」）")
	}
	for _, name := range testCase.ExpectTools {
		if !slices.Contains(run.Tools, name) {
			run.Missing = append(run.Missing, name)
		}
	}
	run.Reminders, run.Memories = frontState(t, ctx, kit, shared.speaker)
	return run
}

// frontBriefers 照服务端的每轮情况说明：时间、常驻记忆、错过的提醒、家大概在哪（额度提醒、任务板在评测里没有内容，不加）。
func frontBriefers(shared frontShared, kit frontCaseKit) []brain.Briefer {
	briefers := []brain.Briefer{brain.ClockBriefer{}, kit.keeper, kit.scheduler}
	if shared.home != nil {
		briefers = append(briefers, brain.BriefLines(weather.LocationNote{Home: shared.home}.Brief))
	}
	return briefers
}

// frontState 是会话结束时这个人在用的提醒和记得的事（给评审核对结果）。
func frontState(t *testing.T, ctx context.Context, kit frontCaseKit, person string) ([]string, []string) {
	t.Helper()
	active, err := kit.reminders.Active(ctx, person)
	if err != nil {
		t.Fatal(err)
	}
	var reminderLines []string
	for _, reminder := range active {
		line := fmt.Sprintf("#%d %s，第一次 %s", reminder.ID, reminder.Text, reminder.StartsAt.In(time.Local).Format("2006-01-02 15:04 Mon"))
		if reminder.Rule != "" {
			line += "，重复 " + reminder.Rule
		}
		reminderLines = append(reminderLines, line)
	}
	list, _, err := kit.notes.List(ctx, person, 50)
	if err != nil {
		t.Fatal(err)
	}
	var memoryLines []string
	for _, note := range list {
		line := fmt.Sprintf("#%d %s（来源 %s", note.ID, note.Content, note.Source)
		if note.Resident {
			line += "，常驻"
		}
		if note.Person == "" {
			line += "，全家共用"
		}
		memoryLines = append(memoryLines, line+"）")
	}
	return reminderLines, memoryLines
}

// collectFrontAnswer 收会话的事件直到 response.done（最多等 timeout）。
func collectFrontAnswer(session *brain.Session, asked time.Time, timeout time.Duration) (string, realtimeproto.ResponseDone,
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
			return braintest.Transcript(events), realtimeproto.ResponseDone{}, first, time.Since(asked), fmt.Errorf("等了 %v 还没答完", timeout)
		}
	}
}

// frontRecorder 包一层前台模型：记下每次请求的输入 token、调了什么工具、工具的结果（从下一次请求的消息里取）、思考字数与费用。
type frontRecorder struct {
	brain.ChatModel
	model   string
	pricing spending.Pricing

	mu       sync.Mutex
	seen     map[string]bool // 已经记过结果的工具调用
	offered  int             // 第一次请求给了几个工具
	trace    []string
	tools    []string
	prompts  []int
	output   int
	thinking int
	usd      float64
}

func (r *frontRecorder) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	r.mu.Lock()
	if r.offered == 0 {
		r.offered = len(request.Tools)
	}
	for _, message := range request.Messages {
		if message.Role == brain.RoleTool && message.ToolCallID != "" && !r.seen[message.ToolCallID] {
			r.seen[message.ToolCallID] = true
			r.trace = append(r.trace, "  → "+message.ToolName+" 的结果："+evalOneLine(message.Text, 260))
		}
	}
	r.mu.Unlock()
	return func(yield func(brain.ReplyDelta, error) bool) {
		for delta, err := range r.ChatModel.StreamReply(ctx, request) {
			r.mu.Lock()
			for _, call := range delta.ToolCalls {
				arguments, _ := json.Marshal(call.Arguments)
				r.trace = append(r.trace, "调用 "+call.Name+" "+evalOneLine(string(arguments), 200))
				r.tools = append(r.tools, call.Name)
			}
			if delta.Usage != nil {
				r.prompts = append(r.prompts, delta.Usage.PromptTokens)
				r.output += delta.Usage.OutputTokens
				r.thinking += delta.Usage.ThinkingChars
				r.usd += r.pricing.LLMCost(r.model, *delta.Usage).Value()
			}
			r.mu.Unlock()
			if !yield(delta, err) {
				return
			}
		}
	}
}

func (r *frontRecorder) fill(run *frontRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run.Trace, run.Tools, run.Prompts = slices.Clone(r.trace), slices.Clone(r.tools), slices.Clone(r.prompts)
	run.Output, run.Thinking, run.USD, run.Offered = r.output, r.thinking, r.usd, r.offered
}

// fixedAnswerModel 是假的强模型：每次都给同一句（不按问的内容）。
type fixedAnswerModel struct{ text string }

func (f fixedAnswerModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		if !yield(brain.ReplyDelta{Text: f.text}, nil) {
			return
		}
		yield(brain.ReplyDelta{Usage: &brain.ReplyUsage{}, Stop: brain.StopCompleted}, nil)
	}
}

// recordedDesk 是假的后台调查桌：只记下派了什么调查，不真跑。
type recordedDesk struct {
	mu      sync.Mutex
	started []string
}

func (d *recordedDesk) Start(_ context.Context, _ string, question string) (research.Task, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.started = append(d.started, question)
	return research.Task{ID: int64(len(d.started)), Question: question, StatusText: "调查中", CreatedAt: time.Now()}, 0, nil
}

func (d *recordedDesk) Status(context.Context, string, *int64) (research.StatusReport, error) {
	return research.StatusReport{}, nil
}

func (d *recordedDesk) Cancel(context.Context, string, int64) (research.Task, error) {
	return research.Task{}, errors.New("没有这个调查")
}

func (d *recordedDesk) LongestWait() time.Duration { return 0 }

func (d *recordedDesk) questions() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.started)
}

// frontSearchMeter 是评测用的搜索额度：不拦、不记账。
type frontSearchMeter struct{}

func (frontSearchMeter) AllowSearch(context.Context, int) error        { return nil }
func (frontSearchMeter) RecordSearch(context.Context, int, bool) error { return nil }

// frontTextAudio 把要念的句子原样当成声音（评测只看文字）。
type frontTextAudio struct{}

func (frontTextAudio) Synthesize(_ context.Context, utterance brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		yield(realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1}, Data: []byte(utterance.Text)}, nil)
	}
}

// noThinkTransport 给发往本机模型的每个请求加上 chat_template_kwargs.enable_thinking = false（llama.cpp 等按 Qwen 一类
// 聊天模板关掉思考的写法）。只在评测里用。
type noThinkTransport struct{ base http.RoundTripper }

func (n noThinkTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body == nil || request.Method != http.MethodPost {
		return n.base.RoundTrip(request)
	}
	body, err := io.ReadAll(request.Body)
	_ = request.Body.Close()
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		payload["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
		if rewritten, err := json.Marshal(payload); err == nil {
			body = rewritten
		}
	}
	clone := request.Clone(request.Context())
	clone.Body, clone.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return n.base.RoundTrip(clone)
}

func loadFrontCases(t *testing.T, dir string) []frontCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "cases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []frontCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, testCase := range file.Cases {
		if testCase.ID == "" || len(testCase.Turns) == 0 || seen[testCase.ID] {
			t.Fatalf("cases.yaml：每题要有不重复的 id 和至少一句 turns：%+v", testCase)
		}
		seen[testCase.ID] = true
	}
	if len(file.Cases) == 0 {
		t.Fatal("cases.yaml 里没有题")
	}
	return file.Cases
}

func pickFrontCases(t *testing.T, cases []frontCase) []frontCase {
	t.Helper()
	wanted := evalList(os.Getenv("HARO_EVAL_CASES"))
	if len(wanted) == 0 {
		return cases
	}
	var picked []frontCase
	for _, id := range wanted {
		index := slices.IndexFunc(cases, func(testCase frontCase) bool { return testCase.ID == id })
		if index < 0 {
			t.Fatalf("HARO_EVAL_CASES 里的 %s 不在 cases.yaml", id)
		}
		picked = append(picked, cases[index])
	}
	return picked
}

func pickFrontGroups(t *testing.T) []string {
	t.Helper()
	all := []string{groupHaiku, groupLocal, groupLocalThink}
	wanted := evalList(os.Getenv("HARO_EVAL_GROUPS"))
	if len(wanted) == 0 {
		return all
	}
	for _, name := range wanted {
		if !slices.Contains(all, name) {
			t.Fatalf("HARO_EVAL_GROUPS 只能是 %s：%q", strings.Join(all, "、"), name)
		}
	}
	return wanted
}

func maxInt(values []int) int {
	best := 0
	for _, value := range values {
		best = max(best, value)
	}
	return best
}

// ——— 判分 ———

func TestFrontEvalJudge(t *testing.T) {
	dir, key := os.Getenv("HARO_EVAL_DIR"), os.Getenv("HARO_ANTHROPIC_API_KEY")
	if dir == "" || key == "" {
		t.Skip("没设 HARO_EVAL_DIR / HARO_ANTHROPIC_API_KEY")
	}
	cases := map[string]frontCase{}
	for _, testCase := range loadFrontCases(t, dir) {
		cases[testCase.ID] = testCase
	}
	runPath := latestRun(t, dir)
	runs := loadFrontRuns(t, runPath)
	var settings config.ServerSettings
	if err := env.Parse(&settings); err != nil {
		t.Fatal(err)
	}
	judgeModel := cmp.Or(os.Getenv("HARO_EVAL_JUDGE_MODEL"), "claude-sonnet-5-5")
	judge := newEvalJudge(t, settings, key, judgeModel, llm.ThinkingBetweenTools, 1000)
	verdicts := make([]evalVerdict, len(runs))
	for i, run := range runs {
		verdicts[i] = judgeFrontRun(t, judge, cases[run.Case], run)
		t.Logf("%s %s：%s %s", run.Case, frontGroupLabels[run.Group], verdicts[i].mark(), verdicts[i].Reason)
	}
	report := frontReport(cases, runs, verdicts, filepath.Base(runPath), judgeModel, judge.spent())
	path := filepath.Join(dir, "results", "report-"+time.Now().Format("20060102-1504")+".md")
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("报告写到 %s（判分花了 $%.3f）", path, judge.spent())
}

// judgeFrontRun 请评审对照参考答案判一次（看不到是哪一组），经 submit_grade 交出判断。没答完的那一轮也交给评审看（照实判）。
func judgeFrontRun(t *testing.T, judge brain.ChatModel, testCase frontCase, run frontRun) evalVerdict {
	t.Helper()
	var transcript strings.Builder
	for _, turn := range run.Turns {
		fmt.Fprintf(&transcript, "[%s] 主人：%s\n", turn.AskedAt.In(time.Local).Format("2006-01-02 15:04 Mon"), turn.Said)
		answer := cmp.Or(turn.Answer, "（没说话）")
		if turn.Error != "" || turn.Status != string(realtimeproto.StatusCompleted) {
			answer += fmt.Sprintf("（这一轮没正常答完：%s）", cmp.Or(turn.Error, turn.Reason, turn.Status))
		}
		fmt.Fprintf(&transcript, "Haro：%s\n", answer)
	}
	missing := "都调了"
	if len(run.Missing) > 0 {
		missing = "没调：" + strings.Join(run.Missing, "、")
	}
	question := fmt.Sprintf("类别：%s\n\n对话（日本时间；第一句开头的「"+frontWakePhrase+"」是叫醒 Haro 的唤醒词，不是主人要说的内容）：\n%s\n工具调用与结果（按顺序）：\n%s\n\n会话结束时在用的提醒：%s\n会话结束时的记忆：%s\n"+
		"程序核对应调的工具（%s）：%s\n\n参考答案与判分标准：%s", testCase.Category, transcript.String(), cmp.Or(strings.Join(run.Trace, "\n"), "（没调工具）"),
		cmp.Or(strings.Join(run.Reminders, "；"), "（无）"), cmp.Or(strings.Join(run.Memories, "；"), "（无）"),
		cmp.Or(strings.Join(testCase.ExpectTools, "、"), "无要求"), missing, testCase.Expect)
	return askJudge(t, judge, "你是评测的评审：对照参考答案，判断家用语音助手 Haro 在这道题里的表现（工具调用、最后的状态、说给主人的话）。",
		question, "交出判断。correct：做到了参考答案要求的（工具调用、结果、回答都对）；partial：大体对，但缺了一部分或有小错（参考答案里写了算部分对的情况照它）；"+
			"wrong：没做到、做错、编造，或者这一轮没答完（失败、超时、一个字没说）。回答是说给人听的语音，文笔不重要，按参考答案里写的标准判。")
}

func loadFrontRuns(t *testing.T, path string) []frontRun {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var runs []frontRun
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var run frontRun
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

// frontTotals 是一组的汇总。
type frontTotals struct {
	answers, correct, partial, wrong, failed int
	score                                    float64
	firstMs, doneMs                          int64
	turns, firstCount                        int
	maxPrompt                                int
	usd                                      float64
	byCategory                               map[string]float64
	categoryCount                            map[string]int
}

// frontReport 是 Markdown 报告：汇总（含每次请求输入最多多少 token，看 16k 够不够）、按类、逐题、明细。
func frontReport(cases map[string]frontCase, runs []frontRun, verdicts []evalVerdict, runName, judgeModel string, judgeUSD float64) string {
	var report strings.Builder
	fmt.Fprintf(&report, "# 前台能力对比 %s\n\n回答：`%s`；判分：%s（初判，最后以人看为准；判分花了 $%.3f）\n\n",
		time.Now().Format("2006-01-02 15:04"), runName, judgeModel, judgeUSD)
	var groups []string
	for _, group := range []string{groupHaiku, groupLocal, groupLocalThink} {
		if slices.ContainsFunc(runs, func(run frontRun) bool { return run.Group == group }) {
			groups = append(groups, group)
		}
	}
	totals := map[string]*frontTotals{}
	categories := []string{}
	for _, group := range groups {
		totals[group] = &frontTotals{byCategory: map[string]float64{}, categoryCount: map[string]int{}}
	}
	for i, run := range runs {
		sum, verdict := totals[run.Group], verdicts[i]
		sum.answers++
		sum.score += verdict.score()
		sum.byCategory[run.Category] += verdict.score()
		sum.categoryCount[run.Category]++
		if !slices.Contains(categories, run.Category) {
			categories = append(categories, run.Category)
		}
		switch {
		case !verdict.judged:
		case verdict.Grade == gradeCorrect:
			sum.correct++
		case verdict.Grade == gradePartial:
			sum.partial++
		default:
			sum.wrong++
		}
		if run.failed() {
			sum.failed++
		}
		for _, turn := range run.Turns {
			sum.turns++
			sum.doneMs += turn.DoneMs
			if turn.FirstMs > 0 {
				sum.firstMs += turn.FirstMs
				sum.firstCount++
			}
		}
		sum.maxPrompt = max(sum.maxPrompt, maxInt(run.Prompts))
		sum.usd += run.USD
	}
	report.WriteString("## 汇总\n\n| 组 | 得分 | 对 | 部分对 | 错 | 其中没答完 | 第一句（每轮平均） | 答完（每轮平均） | 一次请求的输入最多 | 花费 |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, group := range groups {
		sum := totals[group]
		first := "—"
		if sum.firstCount > 0 {
			first = fmt.Sprintf("%.1f 秒", float64(sum.firstMs)/float64(sum.firstCount)/1000)
		}
		fmt.Fprintf(&report, "| %s | %.1f / %d | %d | %d | %d | %d | %s | %.1f 秒 | %d token | $%.3f |\n", frontGroupLabels[group], sum.score, sum.answers,
			sum.correct, sum.partial, sum.wrong, sum.failed, first, float64(sum.doneMs)/float64(max(sum.turns, 1))/1000, sum.maxPrompt, sum.usd)
	}
	report.WriteString("\n## 按类\n\n| 类 | " + strings.Join(labelsOf(groups), " | ") + " |\n|---|" + strings.Repeat("---|", len(groups)) + "\n")
	for _, category := range categories {
		row := []string{category}
		for _, group := range groups {
			sum := totals[group]
			row = append(row, fmt.Sprintf("%.1f / %d", sum.byCategory[category], sum.categoryCount[category]))
		}
		report.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	ids := slices.Sorted(maps.Keys(cases))
	report.WriteString("\n## 逐题\n\n| 题 | " + strings.Join(labelsOf(groups), " | ") + " |\n|---|" + strings.Repeat("---|", len(groups)) + "\n")
	for _, id := range ids {
		row := []string{id + " " + evalOneLine(strings.Join(cases[id].Turns, " → "), 26)}
		present := false
		for _, group := range groups {
			cell := "—"
			for i, run := range runs {
				if run.Case == id && run.Group == group {
					cell, present = verdicts[i].mark(), true
				}
			}
			row = append(row, cell)
		}
		if present {
			report.WriteString("| " + strings.Join(row, " | ") + " |\n")
		}
	}
	report.WriteString("\n## 明细\n")
	for _, id := range ids {
		testCase := cases[id]
		var lines []string
		for _, group := range groups {
			for i, run := range runs {
				if run.Case != id || run.Group != group {
					continue
				}
				var block strings.Builder
				fmt.Fprintf(&block, "- **%s** %s %s\n", frontGroupLabels[group], verdicts[i].mark(), verdicts[i].Reason)
				for _, turn := range run.Turns {
					fmt.Fprintf(&block, "  - [%s] 主人：%s\n  - Haro（第一句 %.1f 秒、答完 %.1f 秒%s）：%s\n", turn.AskedAt.In(time.Local).Format("15:04:05"),
						turn.Said, float64(turn.FirstMs)/1000, float64(turn.DoneMs)/1000, frontTurnState(turn), cmp.Or(evalOneLine(turn.Answer, 600), "（没说话）"))
				}
				for _, trace := range run.Trace {
					fmt.Fprintf(&block, "  - %s\n", trace)
				}
				fmt.Fprintf(&block, "  - 结束时：提醒 %s；记忆 %s；每次请求的输入 %v token；思考 %d 字；$%.4f\n", cmp.Or(strings.Join(run.Reminders, "；"), "无"),
					cmp.Or(strings.Join(run.Memories, "；"), "无"), run.Prompts, run.Thinking, run.USD)
				lines = append(lines, block.String())
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(&report, "\n### %s（%s）%s\n\n参考：%s\n\n%s", id, testCase.Category, strings.Join(testCase.Turns, " → "), testCase.Expect, strings.Join(lines, ""))
		}
	}
	return report.String()
}

func frontTurnState(turn frontTurn) string {
	if turn.Error != "" {
		return "，失败：" + turn.Error
	}
	if turn.Status != string(realtimeproto.StatusCompleted) {
		return "，没答完：" + cmp.Or(turn.Reason, turn.Status)
	}
	return ""
}

func labelsOf(groups []string) []string {
	labels := make([]string, 0, len(groups))
	for _, group := range groups {
		labels = append(labels, frontGroupLabels[group])
	}
	return labels
}
