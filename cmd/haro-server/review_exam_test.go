//go:build eval

// 只考审查员：同一批草稿（有的原样、有的埋了一处错）交给几组设置的审查员各审一次，看埋的错抓不抓得到、原样的会不会被挑出疑点。
// 审查员照线上装配（researchAgents），审查走线上的同一段代码（research.ExamReview），不经过调查：草稿和来源的内容都是固定的，
// 几组看到的完全一样。组的名字和设置与后台调查对比（research_eval_test.go）相同，HARO_EVAL_ARMS 挑组。
//
// 题目不放在这个仓库：HARO_EVAL_DIR 指向题目目录（review-exam.yaml），结果写到 results/：
//
//  1. TestReviewExamPages：按线上调查读网页的方式（按问题挑相关段落、每页字数一样）把每题的来源读一次，
//     写成 results/review-exam-pages.json；之后几组都用这份，重跑不再读。
//
//  2. TestReviewExamRun：每份草稿各组审一次，写 exam-<时间>.jsonl（通过还是疑点、逐条疑点、用时、费用）；
//     累计花费超过 HARO_EVAL_BUDGET_USD（默认 2）就停。
//
//  3. TestReviewExamJudge：评审（默认 Opus 5.5）看埋了什么错和审查员交回的疑点，判有没有指到那处错（看不到是哪一组），
//     写 exam-report-<时间>.md。原样的草稿不判，只数疑点。
//
//     HARO_ANTHROPIC_API_KEY=… HARO_TAVILY_API_KEY=… HARO_CLOUD_LLM_MODEL=claude-haiku-5-5 HARO_STRONG_LLM_MODEL=claude-sonnet-5-5 \
//     HARO_EVAL_DIR=… go test -tags eval -run ReviewExamPages -v -count=1 ./cmd/haro-server/
//     HARO_EVAL_ARMS=now,think-base … go test -tags eval -run ReviewExamRun -v -count=1 -timeout 60m ./cmd/haro-server/
//     HARO_ANTHROPIC_API_KEY=… HARO_EVAL_DIR=… [HARO_EVAL_RUN=…/exam-….jsonl] go test -tags eval -run ReviewExamJudge -v -count=1 ./cmd/haro-server/
package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/money"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/spending"
)

const examPagesFile = "review-exam-pages.json"

// examFile 是 review-exam.yaml：today 是交给审查员的「现在的日期」（照草稿写成的那天）。
type examFile struct {
	Today string     `yaml:"today"`
	Items []examItem `yaml:"items"`
}

// examItem 是一道题：问题、草稿列出的来源，和几份草稿。
type examItem struct {
	ID       string      `yaml:"id"`
	Question string      `yaml:"question"`
	Sources  []string    `yaml:"sources"`
	Drafts   []examDraft `yaml:"drafts"`
}

// examDraft 是一份草稿：Planted 是埋了什么错（原样的不写）。
type examDraft struct {
	ID      string `yaml:"id"`
	Planted string `yaml:"planted"`
	Text    string `yaml:"text"`
}

func loadExam(t *testing.T, dir string) (examFile, time.Time) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "review-exam.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file examFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	today, err := time.ParseInLocation(time.DateOnly, file.Today, time.Local)
	if err != nil {
		t.Fatalf("review-exam.yaml 的 today 要写成 2006-01-02：%v", err)
	}
	seen := map[string]bool{}
	for _, item := range file.Items {
		if item.ID == "" || item.Question == "" || len(item.Sources) == 0 || len(item.Drafts) == 0 || seen[item.ID] {
			t.Fatalf("review-exam.yaml：每题要有不重复的 id、question、sources 和 drafts：%s", item.ID)
		}
		seen[item.ID] = true
		drafts := map[string]bool{}
		for _, draft := range item.Drafts {
			if draft.ID == "" || strings.TrimSpace(draft.Text) == "" || drafts[draft.ID] {
				t.Fatalf("review-exam.yaml：%s 的每份草稿要有不重复的 id 和 text", item.ID)
			}
			drafts[draft.ID] = true
		}
	}
	return file, today
}

// ——— 1. 读来源 ———

func TestReviewExamPages(t *testing.T) {
	dir := os.Getenv("HARO_EVAL_DIR")
	var settings config.ServerSettings
	if err := env.Parse(&settings); err != nil {
		t.Fatal(err)
	}
	if dir == "" || settings.TavilyAPIKey == "" {
		t.Skip("没设 HARO_EVAL_DIR / HARO_TAVILY_API_KEY")
	}
	file, _ := loadExam(t, dir)
	logger := slog.New(slog.DiscardHandler)
	searcher := connectWebSearch(settings, &http.Client{}, logger)
	defer searcher.Close()
	reader := researchSearch{searcher: searcher}
	credits := &creditCounter{}
	pages := map[string][]research.Page{}
	for _, item := range file.Items {
		for batch := range slices.Chunk(item.Sources, 3) { // 和调查一样，一次最多读 3 页
			read, err := reader.ReadPages(context.Background(), batch, research.ExamPageRunes, item.Question, credits)
			if err != nil {
				t.Fatalf("%s 读网页失败：%v", item.ID, err)
			}
			pages[item.ID] = append(pages[item.ID], read...)
		}
		for _, page := range pages[item.ID] {
			note := ""
			if page.Error != "" {
				note = "（没读到：" + page.Error + "）"
			}
			t.Logf("%s %s：%d 字%s", item.ID, page.URL, len([]rune(page.Content)), note)
		}
	}
	data, err := json.MarshalIndent(pages, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "results", examPagesFile)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("写到 %s（搜索积分 %d）", path, credits.total())
}

// creditCounter 数读网页用了多少积分。
type creditCounter struct {
	mu      sync.Mutex
	credits int
}

func (c *creditCounter) RecordSearch(_ context.Context, credits int, _ bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credits += credits
	return nil
}

func (c *creditCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.credits
}

func loadExamPages(t *testing.T, dir string) map[string][]research.Page {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "results", examPagesFile))
	if err != nil {
		t.Fatalf("先跑 TestReviewExamPages：%v", err)
	}
	var pages map[string][]research.Page
	if err := json.Unmarshal(data, &pages); err != nil {
		t.Fatal(err)
	}
	return pages
}

// ——— 2. 审 ———

// examRun 是一份草稿一组审一次的结果。
type examRun struct {
	Item      string               `json:"item"`
	Draft     string               `json:"draft"`
	Planted   string               `json:"planted,omitempty"`
	Arm       string               `json:"arm"`
	Reviewer  string               `json:"reviewer"`
	Pass      bool                 `json:"pass"`
	Doubts    []research.ExamDoubt `json:"doubts,omitempty"`
	Error     string               `json:"error,omitempty"`
	ElapsedMs int64                `json:"elapsed_ms"`
	USD       float64              `json:"usd"`
	Tokens    int                  `json:"tokens"`
}

// examReviewer 是一组的审查员：照线上装配出的代理，和它的账本（数花了多少）。
type examReviewer struct {
	agents research.Agents
	model  string
	ledger *recordingLedger
}

func TestReviewExamRun(t *testing.T) {
	dir, key := os.Getenv("HARO_EVAL_DIR"), os.Getenv("HARO_ANTHROPIC_API_KEY")
	if dir == "" || key == "" {
		t.Skip("没设 HARO_EVAL_DIR / HARO_ANTHROPIC_API_KEY")
	}
	file, today := loadExam(t, dir)
	pages := loadExamPages(t, dir)
	arms := pickResearchArms(t)
	var base config.ServerSettings
	if err := env.Parse(&base); err != nil {
		t.Fatal(err)
	}
	if base.CloudLLMAPI != config.CloudAPIAnthropic || base.StrongLLMModel == "" {
		t.Fatal("要用 Claude 的接口，并设好 HARO_STRONG_LLM_MODEL")
	}
	budget := envFloat(t, "HARO_EVAL_BUDGET_USD", 2)
	thinkEffort := cmp.Or(os.Getenv("HARO_EVAL_THINK_EFFORT"), "xhigh")
	thinkOutput := int(envFloat(t, "HARO_EVAL_THINK_OUTPUT_TOKENS", 16000))
	shared := newResearchShared(t, base, key)
	reviewers := map[string]examReviewer{}
	for _, arm := range arms {
		settings := researchArmSettings(t, base, arm, thinkEffort, thinkOutput)
		reviewers[arm] = buildExamReviewer(t, shared, settings)
		t.Logf("%s：%s", researchArmLabels[arm], describeResearchSettings(settings))
	}

	out, err := os.Create(filepath.Join(dir, "results", "exam-"+time.Now().Format("20060102-1504")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	spent := 0.0
	for _, item := range file.Items {
		for _, draft := range item.Drafts {
			runs := make([]examRun, len(arms))
			var wg sync.WaitGroup
			for i, arm := range arms {
				wg.Go(func() { runs[i] = runExamReview(reviewers[arm], arm, today, item, draft, pages[item.ID]) })
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
				outcome := fmt.Sprintf("疑点 %d", len(run.Doubts))
				if run.Pass {
					outcome = "通过"
				}
				if run.Error != "" {
					outcome = "没审成：" + run.Error
				}
				t.Logf("%s/%s %s：%s，%.0f 秒，$%.4f", run.Item, run.Draft, researchArmLabels[run.Arm], outcome, float64(run.ElapsedMs)/1000, run.USD)
			}
			if spent > budget {
				t.Fatalf("累计 $%.2f 超过上限 $%.2f：停下（已跑的在 %s）", spent, budget, out.Name())
			}
		}
	}
	t.Logf("跑完：%s，共 $%.2f", out.Name(), spent)
}

// buildExamReviewer 照线上装配一组的审查员（researchAgents，模型按调查的用途记账）。
func buildExamReviewer(t *testing.T, shared researchShared, settings config.ServerSettings) examReviewer {
	t.Helper()
	ledger, err := spending.OpenLedger(context.Background(), "") // 账本只放内存：只为数花了多少
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	recorded := &recordingLedger{Ledger: ledger}
	logger := slog.New(slog.DiscardHandler)
	assembly := agentAssembly{settings: settings, catalog: shared.catalog, claude: shared.api, client: &http.Client{},
		pricing: shared.pricing, ledger: recorded, guard: spending.NewGuard(recorded, spending.Limits{}, money.SameCurrencyOnly{}, logger), logger: logger}
	built, err := assembly.researchAgents(researchLimits(settings))
	if err != nil {
		t.Fatal(err)
	}
	model := settings.StrongLLMModel
	if settings.ResearchReviewer == config.ReviewerBase {
		model = settings.CloudLLMModel
	}
	return examReviewer{agents: built, model: model, ledger: recorded}
}

// runExamReview 请一组的审查员审一份草稿；花费按这次审查前后账本多出来的记。各组各有账本，同一组同时只审一份。
func runExamReview(reviewer examReviewer, arm string, today time.Time, item examItem, draft examDraft, pages []research.Page) examRun {
	run := examRun{Item: item.ID, Draft: draft.ID, Planted: draft.Planted, Arm: arm, Reviewer: reviewer.model}
	reviewer.ledger.mu.Lock()
	before := len(reviewer.ledger.charges)
	reviewer.ledger.mu.Unlock()
	started := time.Now()
	verdict, err := research.ExamReview(context.Background(), reviewer.agents, today, item.Question, draft.Text, item.Sources, pages)
	run.ElapsedMs = time.Since(started).Milliseconds()
	run.Pass, run.Doubts, run.Tokens = verdict.Pass, verdict.Doubts, verdict.Tokens
	if err != nil {
		run.Error = err.Error()
	}
	reviewer.ledger.mu.Lock()
	defer reviewer.ledger.mu.Unlock()
	for _, charge := range reviewer.ledger.charges[before:] {
		run.USD += charge.Cost.Value() // 按原始的美元记
	}
	return run
}

// ——— 3. 判 ———

const (
	examJudgeRole = "你是评测的评审：检查审查员有没有抓到草稿里故意埋下的一处错。审查员拿到的是问题、草稿和来源的摘录，" +
		"交回「通过」或逐条的疑点；你拿到的是埋了什么错、草稿和审查员交回的结果。"
	examGrades = "交出判断。correct：有一条疑点指到了埋的那处错（说的是那句话或那个说法，并且指出它没依据、和资料矛盾或不准确）；" +
		"partial：有疑点碰到了那句话，但说的是别的问题，或说得含糊、看不出是在指那处错；" +
		"wrong：审查员交了通过，或疑点都和埋的错无关，或没审成。其他疑点对不对不影响判断。"
)

func TestReviewExamJudge(t *testing.T) {
	dir, key := os.Getenv("HARO_EVAL_DIR"), os.Getenv("HARO_ANTHROPIC_API_KEY")
	if dir == "" || key == "" {
		t.Skip("没设 HARO_EVAL_DIR / HARO_ANTHROPIC_API_KEY")
	}
	file, _ := loadExam(t, dir)
	drafts := map[string]examDraft{}
	questions := map[string]string{}
	for _, item := range file.Items {
		questions[item.ID] = item.Question
		for _, draft := range item.Drafts {
			drafts[item.ID+"/"+draft.ID] = draft
		}
	}
	runPath := os.Getenv("HARO_EVAL_RUN")
	if runPath == "" {
		matches, _ := filepath.Glob(filepath.Join(dir, "results", "exam-2*.jsonl"))
		if len(matches) == 0 {
			t.Fatal("results 里没有 exam-*.jsonl")
		}
		slices.Sort(matches)
		runPath = matches[len(matches)-1]
	}
	runs := loadExamRuns(t, runPath)
	var settings config.ServerSettings
	if err := env.Parse(&settings); err != nil {
		t.Fatal(err)
	}
	judgeModel := cmp.Or(os.Getenv("HARO_EVAL_JUDGE_MODEL"), "claude-opus-5-5")
	judge := newEvalJudge(t, settings, key, judgeModel, llm.ThinkingAdaptive, 4000)
	verdicts := make([]evalVerdict, len(runs))
	for i, run := range runs {
		if run.Planted == "" {
			continue
		}
		draft := drafts[run.Item+"/"+run.Draft]
		verdicts[i] = askJudge(t, judge, examJudgeRole, examJudgeMaterial(questions[run.Item], draft, run), examGrades)
		t.Logf("%s/%s %s：%s %s", run.Item, run.Draft, researchArmLabels[run.Arm], verdicts[i].mark(), verdicts[i].Reason)
	}
	report := examReport(runs, verdicts, filepath.Base(runPath), judgeModel, judge.spent())
	path := filepath.Join(dir, "results", "exam-report-"+time.Now().Format("20060102-1504")+".md")
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("报告写到 %s（判分花了 $%.3f）", path, judge.spent())
}

// examJudgeMaterial 是交给评审看的：问题、埋了什么错、草稿、审查员交回的（看不到是哪一组、用了什么模型）。
func examJudgeMaterial(question string, draft examDraft, run examRun) string {
	result := "审查员交了通过（没有疑点）。"
	switch {
	case run.Error != "":
		result = "审查员没审成：" + run.Error
	case !run.Pass:
		lines := make([]string, len(run.Doubts))
		for i, doubt := range run.Doubts {
			lines[i] = fmt.Sprintf("%d.（%s）%s —— 依据：%s", i+1, doubt.Kind, doubt.Claim, doubt.Evidence)
		}
		result = "审查员交回的疑点：\n" + strings.Join(lines, "\n")
	}
	return fmt.Sprintf("问题：%s\n\n埋下的错：%s\n\n草稿：\n%s\n\n%s", question, draft.Planted, draft.Text, result)
}

func loadExamRuns(t *testing.T, path string) []examRun {
	t.Helper()
	data, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	var runs []examRun
	scanner := bufio.NewScanner(data)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var run examRun
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

// examReport 写报告：每组抓到几处埋的错、原样的草稿通过几份、平均几条疑点、用时和花费；再逐份列出。
func examReport(runs []examRun, verdicts []evalVerdict, runName, judgeModel string, judgeUSD float64) string {
	type tally struct {
		planted, caught, partial, missed, failed int
		clean, cleanPass, doubts, reviews        int
		elapsedMs                                int64
		usd                                      float64
	}
	var arms []string
	tallies := map[string]*tally{}
	for i, run := range runs {
		if tallies[run.Arm] == nil {
			tallies[run.Arm] = &tally{}
			arms = append(arms, run.Arm)
		}
		s := tallies[run.Arm]
		s.reviews++
		s.elapsedMs += run.ElapsedMs
		s.usd += run.USD
		s.doubts += len(run.Doubts)
		if run.Error != "" {
			s.failed++
		}
		if run.Planted == "" {
			s.clean++
			if run.Pass && run.Error == "" {
				s.cleanPass++
			}
			continue
		}
		s.planted++
		switch {
		case !verdicts[i].judged:
		case verdicts[i].Grade == gradeCorrect:
			s.caught++
		case verdicts[i].Grade == gradePartial:
			s.partial++
		default:
			s.missed++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 只考审查员：%s\n\n评审 %s（$%.3f）。「抓到」= 有一条疑点指到了埋的错；原样的草稿不判，只数通过几份。\n\n", runName, judgeModel, judgeUSD)
	b.WriteString("| 组 | 审查员 | 埋的错：抓到 / 碰到 / 漏掉 | 原样的通过 | 平均疑点 | 没审成 | 平均用时 | 每份花费 |\n|---|---|---|---|---|---|---|---|\n")
	for _, arm := range arms {
		s := tallies[arm]
		reviewer := runs[slices.IndexFunc(runs, func(run examRun) bool { return run.Arm == arm })].Reviewer
		fmt.Fprintf(&b, "| %s | %s | %d / %d / %d（共 %d） | %d / %d | %.1f | %d | %.0f 秒 | $%.4f |\n", researchArmLabels[arm], reviewer,
			s.caught, s.partial, s.missed, s.planted, s.cleanPass, s.clean, float64(s.doubts)/float64(s.reviews), s.failed,
			float64(s.elapsedMs)/1000/float64(s.reviews), s.usd/float64(s.reviews))
	}
	b.WriteString("\n## 逐份\n\n| 草稿 | 埋的错 | 组 | 结果 | 评审 |\n|---|---|---|---|---|\n")
	for i, run := range runs {
		outcome := fmt.Sprintf("疑点 %d", len(run.Doubts))
		if run.Pass {
			outcome = "通过"
		}
		if run.Error != "" {
			outcome = "没审成"
		}
		judged := "—"
		if run.Planted != "" {
			judged = verdicts[i].mark() + " " + evalOneLine(verdicts[i].Reason, 80)
		}
		fmt.Fprintf(&b, "| %s/%s | %s | %s | %s | %s |\n", run.Item, run.Draft, cmp.Or(evalOneLine(run.Planted, 40), "（原样）"),
			researchArmLabels[run.Arm], outcome, judged)
	}
	return b.String()
}
