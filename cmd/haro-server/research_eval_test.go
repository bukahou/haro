//go:build eval

// 后台调查对比：同一批调查题由几种设置各跑一次真的后台调查（线上的代理定义和装配 startResearch，真的 Claude 和 Tavily；
// 不经前台，题目直接交给调查），比较结论对不对、用了多久、花了多少。
//
// 组（HARO_EVAL_ARMS，逗号分隔，默认全部）：
//
//	now        现在的设置（照服务端配置：两个代理平常的思考写法、HARO_RESEARCH_EFFORT、HARO_RESEARCH_REVIEWER）
//	think      两个代理开着思考（adaptive），力度 HARO_EVAL_THINK_EFFORT（默认 xhigh），审查用更强的模型
//	think-base 同 think，审查改用云端模型
//
// 开着思考的两组放宽上限（每次输出 HARO_EVAL_THINK_OUTPUT_TOKENS，默认 16000；时限与累计 token 见 think* 常量）：
// 先量开着思考实际要多少，线上的值按实测再定。轮数、搜索次数各组一样（服务端配置）。
//
// 题目不放在这个仓库（有家附近的信息）：HARO_EVAL_DIR 指向题目目录（cases.yaml），结果写到 results/：
//
//  1. TestResearchEvalRun：每题各组同时跑（各用各的任务账本、搜索连接、账本），边跑边写 run-<时间>.jsonl
//     （结论、来源、状态、用时、调查的经过、token、费用、搜索积分）；累计花费超过 HARO_EVAL_BUDGET_USD（默认 5）就停。
//     HARO_EVAL_CASES 挑题。
//
//  2. TestResearchEvalJudge：评审（默认 Opus 5.5，HARO_EVAL_JUDGE_MODEL）对照参考答案判（看不到是哪一组），
//     写 report-<时间>.md。评审的判断只是初判。
//
//     HARO_ANTHROPIC_API_KEY=… HARO_TAVILY_API_KEY=… HARO_CLOUD_LLM_MODEL=claude-haiku-5-5 HARO_STRONG_LLM_MODEL=claude-sonnet-5-5 \
//     HARO_EVAL_DIR=… go test -tags eval -run ResearchEvalRun -v -count=1 -timeout 240m ./cmd/haro-server/
//     HARO_ANTHROPIC_API_KEY=… HARO_EVAL_DIR=… [HARO_EVAL_RUN=…/run-….jsonl] go test -tags eval -run ResearchEvalJudge -v -count=1 -timeout 60m ./cmd/haro-server/
package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"go.yaml.in/yaml/v3"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/money"
	"github.com/bukahou/haro/internal/pricecatalog"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/spending"
)

// 组的名字（报告里也用）。
const (
	armNow       = "now"
	armThink     = "think"
	armThinkBase = "think-base"
)

var researchArmLabels = map[string]string{armNow: "甲 现在", armThink: "乙 思考", armThinkBase: "丙 思考·Haiku 审查"}

// 开着思考的两组放宽的上限：先量开着思考实际要多少，线上的值按实测再定。
const (
	thinkSearchTimeout  = 10 * time.Minute
	thinkWriteUpTimeout = 3 * time.Minute
	thinkReviewTimeout  = 5 * time.Minute
	thinkRecheckTimeout = 5 * time.Minute
	thinkMaxTokens      = 600_000
)

// researchCase 是 cases.yaml 里的一道题。
type researchCase struct {
	ID       string   `yaml:"id"`
	Question string   `yaml:"question"`
	Expect   string   `yaml:"expect"`
	Sources  []string `yaml:"sources"`
}

// researchRun 是一题一组跑一次的结果。
type researchRun struct {
	Case         string             `json:"case"`
	Arm          string             `json:"arm"`
	Settings     string             `json:"settings"` // 这一组的模型参数（核对用）
	Status       string             `json:"status"`
	Conclusion   string             `json:"conclusion"`
	Sources      []string           `json:"sources"`
	StopReason   string             `json:"stop_reason,omitempty"`
	Error        string             `json:"error,omitempty"`
	ElapsedMs    int64              `json:"elapsed_ms"`
	Summary      map[string]string  `json:"summary"`  // 调查结束时记下的经过（轮数、搜索、读网页、审查……）
	Warnings     []string           `json:"warnings"` // 调查中的警告（审查没做成、没用工具交结论……）
	ModelUSD     map[string]float64 `json:"model_usd"`
	USD          float64            `json:"usd"`
	InputTokens  int                `json:"input_tokens"`
	OutputTokens int                `json:"output_tokens"`
	CacheRead    int                `json:"cache_read_tokens"`
	Credits      int                `json:"credits"` // 搜索积分
}

func TestResearchEvalRun(t *testing.T) {
	dir, key := os.Getenv("HARO_EVAL_DIR"), os.Getenv("HARO_ANTHROPIC_API_KEY")
	if dir == "" || key == "" {
		t.Skip("没设 HARO_EVAL_DIR / HARO_ANTHROPIC_API_KEY")
	}
	cases := pickResearchCases(t, loadResearchCases(t, dir).Cases)
	arms := pickResearchArms(t)
	var base config.ServerSettings
	if err := env.Parse(&base); err != nil {
		t.Fatal(err)
	}
	if base.CloudLLMAPI != config.CloudAPIAnthropic || base.StrongLLMModel == "" || base.TavilyAPIKey == "" {
		t.Fatal("要用 Claude 的接口，并设好 HARO_STRONG_LLM_MODEL、HARO_TAVILY_API_KEY")
	}
	budget := envFloat(t, "HARO_EVAL_BUDGET_USD", 5)
	thinkEffort := cmp.Or(os.Getenv("HARO_EVAL_THINK_EFFORT"), "xhigh")
	thinkOutput := int(envFloat(t, "HARO_EVAL_THINK_OUTPUT_TOKENS", 16000))
	speaker := cmp.Or(os.Getenv("HARO_EVAL_SPEAKER"), "主人")
	settings := map[string]config.ServerSettings{}
	for _, arm := range arms {
		settings[arm] = researchArmSettings(t, base, arm, thinkEffort, thinkOutput)
		t.Logf("%s：%s", researchArmLabels[arm], describeResearchSettings(settings[arm]))
	}
	shared := newResearchShared(t, base, key)

	if err := os.MkdirAll(filepath.Join(dir, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, "results", "run-"+time.Now().Format("20060102-1504")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	spent := 0.0
	for _, testCase := range cases {
		runs := make([]researchRun, len(arms))
		var wg sync.WaitGroup
		for i, arm := range arms {
			wg.Go(func() { runs[i] = runResearchCase(shared, settings[arm], arm, testCase, speaker) })
		}
		wg.Wait()
		for _, run := range runs {
			line, err := json.Marshal(run)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := out.Write(append(line, '\n')); err != nil {
				t.Fatal(err)
			}
			spent += run.USD
			t.Logf("%s %s：%s，%.0f 秒，%s 轮、搜 %s 次、读 %s 次，审查 %s（疑点 %s），$%.4f，积分 %d%s：%s", run.Case, researchArmLabels[run.Arm],
				cmp.Or(run.Status, "没结束"), float64(run.ElapsedMs)/1000, run.Summary["rounds"], run.Summary["searches"], run.Summary["reads"],
				run.Summary["review"], run.Summary["doubts"], run.USD, run.Credits, researchErrorNote(run), evalOneLine(run.Conclusion, 160))
		}
		if spent > budget {
			t.Fatalf("累计 $%.2f 超过上限 $%.2f：停下（已跑的在 %s）", spent, budget, out.Name())
		}
	}
	t.Logf("跑完：%s，共 $%.2f", out.Name(), spent)
}

// researchArmSettings 是一组的配置：在服务端配置上改后台调查的设置（任务账本每次跑时另建）。改完按启动时的规则检查一遍。
func researchArmSettings(t *testing.T, base config.ServerSettings, arm, effort string, output int) config.ServerSettings {
	t.Helper()
	settings := base
	if arm == armThink || arm == armThinkBase {
		settings.ResearchThinking, settings.ResearchEffort, settings.ResearchMaxOutputTokens = llm.ThinkingAdaptive, effort, output
		settings.ResearchTimeout, settings.ResearchWriteUpTimeout, settings.ResearchMaxTokens = thinkSearchTimeout, thinkWriteUpTimeout, thinkMaxTokens
		settings.ResearchReviewTimeout, settings.ResearchRecheckTimeout = thinkReviewTimeout, thinkRecheckTimeout
		settings.ResearchReviewer = config.ReviewerStrong
		if arm == armThinkBase {
			settings.ResearchReviewer = config.ReviewerBase
		}
	}
	if !settings.ResearchReviewed() {
		t.Fatalf("%s 组没有审查：HARO_STRONG_LLM_USE 要有 research", arm)
	}
	if err := checkClaudeSettings(settings); err != nil {
		t.Fatalf("%s 组的设置不对：%v", arm, err)
	}
	return settings
}

func describeResearchSettings(settings config.ServerSettings) string {
	reviewer := settings.StrongLLMModel
	if settings.ResearchReviewer == config.ReviewerBase {
		reviewer = settings.CloudLLMModel
	}
	return fmt.Sprintf("调查 %s、审查 %s；思考 %s、力度 %s、每次输出 %s；搜索阶段 %s、写结论 %s、审查 %s、复查 %s、累计 %s token",
		settings.CloudLLMModel, reviewer, cmp.Or(settings.ResearchThinking, "（各用各的）"), settings.ResearchEffort,
		orDefault(settings.ResearchMaxOutputTokens), orDefault(settings.ResearchTimeout), orDefault(settings.ResearchWriteUpTimeout),
		orDefault(settings.ResearchReviewTimeout), orDefault(settings.ResearchRecheckTimeout), orDefault(settings.ResearchMaxTokens))
}

// orDefault 写出一项上限：0 写成「默认」（程序里的默认值或代理定义）。
func orDefault[T int | time.Duration](value T) string {
	if value == 0 {
		return "默认"
	}
	return fmt.Sprint(value)
}

func researchErrorNote(run researchRun) string {
	if run.Error == "" {
		return ""
	}
	return "（出错：" + run.Error + "）"
}

// researchShared 是各组共用的：Claude 客户端（全进程一个）、代理定义、单价（汇率记成 1，花费即美元；搜索积分另记）。
type researchShared struct {
	api     *llm.ClaudeAPI
	catalog agents.Catalog
	pricing spending.Pricing
}

func newResearchShared(t *testing.T, base config.ServerSettings, key string) researchShared {
	t.Helper()
	catalog, err := agents.Load(base.AgentsDir)
	if err != nil {
		t.Fatal(err)
	}
	api, err := llm.NewClaudeAPI(key, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	prices, _, err := pricecatalog.Load(context.Background(), &http.Client{Timeout: 30 * time.Second}, base.ModelPricesURL,
		base.ModelPricesProvider, "")
	if err != nil {
		t.Fatal(err)
	}
	pricing := spending.Pricing{Models: map[string]spending.TokenPrice{}}
	for _, model := range []string{base.CloudLLMModel, base.StrongLLMModel} {
		if pricing.Models[model], err = prices.Price(model); err != nil {
			t.Fatal(err)
		}
	}
	return researchShared{api: api, catalog: catalog, pricing: pricing}
}

// runResearchCase 用一组的配置跑一题：照线上装配（startResearch），各用各的任务账本、搜索连接、账本，等它结束（最长 LongestWait 再多一分钟）。
// 在 goroutine 里跑：出错记在结果里，不让测试失败。
func runResearchCase(shared researchShared, settings config.ServerSettings, arm string, testCase researchCase, speaker string) researchRun {
	run := researchRun{Case: testCase.ID, Arm: arm, Settings: describeResearchSettings(settings), ModelUSD: map[string]float64{}}
	storeDir, err := os.MkdirTemp("", "haro-research-eval-")
	if err != nil {
		run.Error = err.Error()
		return run
	}
	defer os.RemoveAll(storeDir)
	settings.ResearchDB = filepath.Join(storeDir, "research.db") // 照线上用 SQLite 的任务账本，每次一份新的
	capture := &researchCapture{}
	logger := slog.New(capture)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ledger, err := spending.OpenLedger(ctx, "") // 账本只放内存：只为数这次花了多少
	if err != nil {
		run.Error = err.Error()
		return run
	}
	defer ledger.Close()
	recorded := &recordingLedger{Ledger: ledger}
	assembly := agentAssembly{settings: settings, catalog: shared.catalog, claude: shared.api, client: &http.Client{},
		pricing: shared.pricing, ledger: recorded, guard: spending.NewGuard(recorded, spending.Limits{}, money.SameCurrencyOnly{}, logger), logger: logger}
	searcher := connectWebSearch(settings, &http.Client{}, logger)
	defer searcher.Close()
	runner, stop, err := startResearch(ctx, settings, assembly, searcher, logger)
	if err != nil {
		run.Error = err.Error()
		return run
	}
	defer func() {
		cancel() // 先停调查的循环，再关任务账本
		_ = stop()
	}()
	finished := make(chan research.Task, 1)
	runner.OnFinished(func(task research.Task) { finished <- task })
	started := time.Now()
	if _, _, err := runner.Start(ctx, speaker, testCase.Question); err != nil {
		run.Error = err.Error()
		return run
	}
	select {
	case task := <-finished:
		run.Status, run.Conclusion, run.Sources, run.StopReason = string(task.Status), task.Conclusion, task.Sources, task.StopReason
	case <-time.After(runner.LongestWait() + time.Minute):
		run.Error = "超过最长等待还没结束"
	}
	run.ElapsedMs = time.Since(started).Milliseconds()
	run.Summary, run.Warnings = capture.result()
	recorded.mu.Lock()
	defer recorded.mu.Unlock()
	for _, charge := range recorded.charges {
		if charge.Credits > 0 {
			run.Credits += charge.Credits
			continue
		}
		run.USD += charge.Cost.Value()
		run.ModelUSD[strings.TrimPrefix(charge.Item, "llm:")] += charge.Cost.Value()
		run.InputTokens += charge.InputTokens
		run.OutputTokens += charge.OutputTokens
		run.CacheRead += charge.CacheReadTokens
	}
	return run
}

// researchCapture 收下调查的日志里要记进结果的：调查结束时那一行的各项（按带着 rounds 这个字段认，不看文字），和警告。
type researchCapture struct {
	mu       sync.Mutex
	summary  map[string]string
	warnings []string
}

func (c *researchCapture) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (c *researchCapture) Handle(_ context.Context, record slog.Record) error {
	fields := map[string]string{}
	var pairs []string
	record.Attrs(func(attr slog.Attr) bool {
		fields[attr.Key] = attr.Value.String()
		pairs = append(pairs, attr.Key+"="+attr.Value.String())
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, summary := fields["rounds"]; summary {
		c.summary = fields
	}
	if record.Level >= slog.LevelWarn {
		c.warnings = append(c.warnings, evalOneLine(record.Message+" "+strings.Join(pairs, " "), 400))
	}
	return nil
}

func (c *researchCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *researchCapture) WithGroup(string) slog.Handler      { return c }

func (c *researchCapture) result() (map[string]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.summary, slices.Clone(c.warnings)
}

// researchCaseFile 是 cases.yaml：题目，和参考答案是哪天核对的（「现在」的题按那天判）。
type researchCaseFile struct {
	Checked string         `yaml:"checked"`
	Cases   []researchCase `yaml:"cases"`
}

func loadResearchCases(t *testing.T, dir string) researchCaseFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "cases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file researchCaseFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, testCase := range file.Cases {
		if testCase.ID == "" || testCase.Question == "" || testCase.Expect == "" || seen[testCase.ID] {
			t.Fatalf("cases.yaml：每题要有不重复的 id、question 和 expect：%+v", testCase)
		}
		seen[testCase.ID] = true
	}
	if len(file.Cases) == 0 || file.Checked == "" {
		t.Fatal("cases.yaml 里要有题和 checked（参考答案是哪天核对的）")
	}
	return file
}

func pickResearchCases(t *testing.T, cases []researchCase) []researchCase {
	t.Helper()
	wanted := evalList(os.Getenv("HARO_EVAL_CASES"))
	if len(wanted) == 0 {
		return cases
	}
	var picked []researchCase
	for _, id := range wanted {
		index := slices.IndexFunc(cases, func(testCase researchCase) bool { return testCase.ID == id })
		if index < 0 {
			t.Fatalf("HARO_EVAL_CASES 里的 %s 不在 cases.yaml", id)
		}
		picked = append(picked, cases[index])
	}
	return picked
}

func pickResearchArms(t *testing.T) []string {
	t.Helper()
	all := []string{armNow, armThink, armThinkBase}
	wanted := evalList(os.Getenv("HARO_EVAL_ARMS"))
	if len(wanted) == 0 {
		return all
	}
	for _, arm := range wanted {
		if !slices.Contains(all, arm) {
			t.Fatalf("HARO_EVAL_ARMS 只能是 %s：%q", strings.Join(all, "、"), arm)
		}
	}
	return wanted
}

// ——— 判分 ———

const (
	researchJudgeRole = "你是评测的评审：对照参考答案，判断家用语音助手 Haro 的后台调查交出的结论对不对。"
	researchGrades    = "交出判断。correct：结论答到了参考答案要求的要点，没有和参考答案矛盾的说法；" +
		"partial：大体对，但缺了要点或有小错（参考答案里写了算部分对的情况照它）；" +
		"wrong：答错、编造、答非所问，或者没交出结论（失败、超时）。结论是调查报告，详略和措辞不重要；" +
		"参考答案没提到的额外内容不扣分，除非它是错的。"
)

func TestResearchEvalJudge(t *testing.T) {
	dir, key := os.Getenv("HARO_EVAL_DIR"), os.Getenv("HARO_ANTHROPIC_API_KEY")
	if dir == "" || key == "" {
		t.Skip("没设 HARO_EVAL_DIR / HARO_ANTHROPIC_API_KEY")
	}
	file := loadResearchCases(t, dir)
	cases := map[string]researchCase{}
	for _, testCase := range file.Cases {
		cases[testCase.ID] = testCase
	}
	runPath := latestRun(t, dir)
	runs := loadResearchRuns(t, runPath)
	var settings config.ServerSettings
	if err := env.Parse(&settings); err != nil {
		t.Fatal(err)
	}
	judgeModel := cmp.Or(os.Getenv("HARO_EVAL_JUDGE_MODEL"), "claude-opus-5-5")
	judge := newEvalJudge(t, settings, key, judgeModel, llm.ThinkingAdaptive, 4000)
	verdicts := make([]evalVerdict, len(runs))
	for i, run := range runs {
		verdicts[i] = askJudge(t, judge, researchJudgeRole, researchJudgeMaterial(cases[run.Case], run, file.Checked), researchGrades)
		t.Logf("%s %s：%s %s", run.Case, researchArmLabels[run.Arm], verdicts[i].mark(), verdicts[i].Reason)
	}
	report := researchReport(cases, runs, verdicts, filepath.Base(runPath), judgeModel, judge.spent())
	path := filepath.Join(dir, "results", "report-"+time.Now().Format("20060102-1504")+".md")
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("报告写到 %s（判分花了 $%.3f）", path, judge.spent())
}

// researchJudgeMaterial 是交给评审看的：问题、调查交出的结论和来源、状态，参考答案（看不到是哪一组、用了什么模型）。
func researchJudgeMaterial(testCase researchCase, run researchRun, checked string) string {
	state := cmp.Or(research.Status(run.Status).Label(), "没结束")
	if run.Error != "" {
		state += "；出错：" + run.Error
	}
	if run.StopReason != "" {
		state += "；说明：" + run.StopReason
	}
	sources := "（没列来源）"
	if len(run.Sources) > 0 {
		sources = "- " + strings.Join(run.Sources, "\n- ")
	}
	return fmt.Sprintf("要调查的问题：%s\n\n调查交出的结论（%s）：\n%s\n\n结论列出的来源：\n%s\n\n参考答案与判分标准（%s 核对）：%s",
		testCase.Question, state, cmp.Or(run.Conclusion, "（没交出结论）"), sources, checked, testCase.Expect)
}

func loadResearchRuns(t *testing.T, path string) []researchRun {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var runs []researchRun
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		var run researchRun
		if err := json.Unmarshal(scanner.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return runs
}

// researchArmStats 是一组的合计。
type researchArmStats struct {
	runs, correct, partial, wrong, unjudged, unfinished int
	score, usd                                          float64
	credits, output                                     int
	elapsed                                             []int64
	rounds, searches, reads                             int
	reviews                                             map[string]int
}

func researchReport(cases map[string]researchCase, runs []researchRun, verdicts []evalVerdict, runName, judgeModel string, judgeUSD float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 后台调查对比 %s\n\n回答：`%s`；判分：%s（初判，最后以人看为准；判分花了 $%.3f）\n\n",
		time.Now().Format("2006-01-02 15:04"), runName, judgeModel, judgeUSD)
	stats := map[string]*researchArmStats{}
	var arms, caseOrder []string
	byCase := map[string]map[string]int{} // 题 → 组 → runs 的下标
	for i, run := range runs {
		s := stats[run.Arm]
		if s == nil {
			s = &researchArmStats{reviews: map[string]int{}}
			stats[run.Arm] = s
			arms = append(arms, run.Arm)
		}
		if byCase[run.Case] == nil {
			byCase[run.Case] = map[string]int{}
			caseOrder = append(caseOrder, run.Case)
		}
		byCase[run.Case][run.Arm] = i
		verdict := verdicts[i]
		s.runs++
		s.score += verdict.score()
		switch {
		case !verdict.judged:
			s.unjudged++
		case verdict.Grade == gradeCorrect:
			s.correct++
		case verdict.Grade == gradePartial:
			s.partial++
		default:
			s.wrong++
		}
		if run.Status != string(research.StatusDone) {
			s.unfinished++
		}
		s.usd += run.USD
		s.credits += run.Credits
		s.output += run.OutputTokens
		s.elapsed = append(s.elapsed, run.ElapsedMs)
		s.rounds += atoiOrZero(run.Summary["rounds"])
		s.searches += atoiOrZero(run.Summary["searches"])
		s.reads += atoiOrZero(run.Summary["reads"])
		s.reviews[cmp.Or(run.Summary["review"], "（没记下）")]++
	}
	slices.SortFunc(arms, func(a, b string) int {
		order := []string{armNow, armThink, armThinkBase}
		return slices.Index(order, a) - slices.Index(order, b)
	})
	b.WriteString("## 汇总\n\n| 组 | 得分 | 对 | 部分对 | 错 | 未判 | 没查完 | 用时（平均 / 最长） | 平均轮数 / 搜索 / 读网页 | 审查 | 每题花费 | 每题输出 token | 搜索积分 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, arm := range arms {
		s := stats[arm]
		n := float64(s.runs)
		var total int64
		for _, ms := range s.elapsed {
			total += ms
		}
		var reviews []string
		for _, outcome := range slices.Sorted(maps.Keys(s.reviews)) {
			reviews = append(reviews, fmt.Sprintf("%s %d", outcome, s.reviews[outcome]))
		}
		fmt.Fprintf(&b, "| %s | %.1f / %d | %d | %d | %d | %d | %d | %.0f / %.0f 秒 | %.1f / %.1f / %.1f | %s | $%.4f | %.0f | %d |\n",
			researchArmLabels[arm], s.score, s.runs, s.correct, s.partial, s.wrong, s.unjudged, s.unfinished,
			float64(total)/n/1000, float64(slices.Max(s.elapsed))/1000, float64(s.rounds)/n, float64(s.searches)/n, float64(s.reads)/n,
			strings.Join(reviews, "、"), s.usd/n, float64(s.output)/n, s.credits)
	}
	b.WriteString("\n## 逐题\n\n| 题 |")
	for _, arm := range arms {
		b.WriteString(" " + researchArmLabels[arm] + " |")
	}
	b.WriteString("\n|---|" + strings.Repeat("---|", len(arms)) + "\n")
	for _, id := range caseOrder {
		fmt.Fprintf(&b, "| %s %s |", id, evalOneLine(cases[id].Question, 30))
		for _, arm := range arms {
			if i, ok := byCase[id][arm]; ok {
				fmt.Fprintf(&b, " %s（%.0f 秒）|", verdicts[i].mark(), float64(runs[i].ElapsedMs)/1000)
			} else {
				b.WriteString(" — |")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("\n## 明细\n")
	for _, id := range caseOrder {
		fmt.Fprintf(&b, "\n### %s %s\n\n参考：%s\n\n", id, cases[id].Question, evalOneLine(cases[id].Expect, 400))
		for _, arm := range arms {
			i, ok := byCase[id][arm]
			if !ok {
				continue
			}
			run := runs[i]
			fmt.Fprintf(&b, "- **%s** %s：%s\n  - %s，%.0f 秒，%s 轮、搜 %s 次、读 %s 次，审查 %s（疑点 %s），$%.4f，积分 %d%s\n  - 结论：%s\n",
				researchArmLabels[arm], verdicts[i].mark(), verdicts[i].Reason, cmp.Or(research.Status(run.Status).Label(), "没结束"),
				float64(run.ElapsedMs)/1000, run.Summary["rounds"], run.Summary["searches"], run.Summary["reads"], run.Summary["review"],
				run.Summary["doubts"], run.USD, run.Credits, researchErrorNote(run), evalOneLine(run.Conclusion, 500))
			if len(run.Warnings) > 0 {
				fmt.Fprintf(&b, "  - 警告：%s\n", strings.Join(run.Warnings, "；"))
			}
		}
	}
	return b.String()
}

func atoiOrZero(text string) int {
	value, _ := strconv.Atoi(text)
	return value
}
