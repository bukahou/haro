package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/agents"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/llm"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/spending"
	"github.com/bukahou/haro/internal/tools"
)

// 后台调查的代理按定义装配：正文填好（工具名、上限由代码给），系统提示词里没有每天变的日期；有审查时（HARO_STRONG_LLM_USE 有
// research）才有审查的代理，正文里才讲复查。轮数默认是定义的 max_steps，设了 HARO_RESEARCH_MAX_ROUNDS 时用它。
func TestBuildResearchAgents(t *testing.T) {
	reviewed, err := testAssembly(t, cloudSettings([]string{config.StrongForResearch}, config.VisionByExpert), nil).
		researchAgents(research.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	investigator := reviewed.Investigator
	if investigator.Name != "调查" || investigator.MaxSteps != 10 || strings.Contains(investigator.Instructions, "{{") ||
		strings.Contains(investigator.Instructions, time.Now().Format("2006年1月2日")) {
		t.Errorf("调查：%+v", investigator)
	}
	for _, tool := range research.InvestigationTools() {
		if !strings.Contains(investigator.Instructions, tool) {
			t.Errorf("调查的正文里应有工具名 %s：%s", tool, investigator.Instructions)
		}
	}
	if !strings.Contains(investigator.Instructions, "复查") {
		t.Errorf("有审查时调查的正文要讲复查：%s", investigator.Instructions)
	}
	reviewer := reviewed.Reviewer
	if reviewer == nil || reviewer.Name != "调查审查" || reviewer.MaxSteps != 1 || reviewer.Timeout != 2*time.Minute ||
		!slices.Equal(reviewer.Tools, research.ReviewTools()) || !strings.Contains(reviewer.Instructions, "submit_review") {
		t.Fatalf("审查：%+v", reviewer)
	}

	settings := cloudSettings([]string{config.StrongForCorrection}, config.VisionByExpert)
	settings.ResearchMaxRounds = 4
	alone, err := testAssembly(t, settings, nil).researchAgents(research.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if alone.Reviewer != nil || strings.Contains(alone.Investigator.Instructions, "复查") {
		t.Errorf("research 不在 HARO_STRONG_LLM_USE 里：不审查，正文也不讲复查：%+v", alone)
	}
	if alone.Investigator.MaxSteps != 4 || !strings.Contains(alone.Investigator.Instructions, "最多 4 轮") {
		t.Errorf("设了 HARO_RESEARCH_MAX_ROUNDS：轮数和正文里的上限都用它：%d %s", alone.Investigator.MaxSteps, alone.Investigator.Instructions)
	}
}

// 调查只用云端模型：HARO_LLM_PROVIDER=local 时对话的代理用本地模型，调查的代理照样用云端的。
func TestResearchUsesCloudModelEvenWhenChattingLocally(t *testing.T) {
	settings := cloudSettings(nil, config.VisionByExpert)
	settings.LLMProvider, settings.Model, settings.OllamaURL = config.LLMLocal, "base-model-local", "http://127.0.0.1:9"
	assembly := testAssembly(t, settings, nil)
	assembly.pricing.Models["base-model-local"] = spending.TokenPrice{Free: true}
	if _, id, err := assembly.meteredForResearch(assembly.catalog[agents.Research]); err != nil || id != "base-model" {
		t.Errorf("调查应用云端模型：%q %v", id, err)
	}
	if _, id, err := assembly.metered(assembly.catalog[agents.Front]); err != nil || id != "base-model-local" {
		t.Errorf("对话的前台跟着 HARO_LLM_PROVIDER：%q %v", id, err)
	}
}

// 覆盖的定义里，调查的工具名写错、正文提到没交给它的工具、审查给了调查的工具：起不来。阳性对照：只改对那一处就起得来。
func TestResearchAgentsRejectBadDefinitions(t *testing.T) {
	settings := cloudSettings([]string{config.StrongForResearch}, config.VisionByExpert)
	investigator := "---\ntitle: 调查\nmode: subtask\nmodel: base\npurpose: research\nmax_steps: 10\ntools: [%s]\n---\n%s\n"
	review := "---\ntitle: 调查审查\nmode: subtask\nmodel: strong\npurpose: research\nmax_steps: 1\ntimeout: 60s\ntools: [%s]\n---\n交回用 {{.ReviewTool}}。\n"
	cases := map[string]struct {
		name, text string
		ok         bool
	}{
		"调查的工具名写错":  {agents.Research, fmt.Sprintf(investigator, "search_web, submit_conclusoin", "用 {{.SearchTool}} 搜。"), false},
		"正文提到没给的工具": {agents.Research, fmt.Sprintf(investigator, "search_web, submit_conclusion", "用 {{.SearchTool}} 搜、{{.ReadTool}} 读。"), false},
		"调查原样":      {agents.Research, fmt.Sprintf(investigator, "search_web, submit_conclusion", "用 {{.SearchTool}} 搜、{{.SubmitTool}} 交。"), true},
		"审查给了调查的工具": {agents.ResearchReview, fmt.Sprintf(review, "submit_review, search_web"), false},
		"审查原样":      {agents.ResearchReview, fmt.Sprintf(review, "submit_review"), true},
	}
	for label, c := range cases {
		_, err := testAssembly(t, settings, map[string]string{c.name: c.text}).researchAgents(research.DefaultLimits)
		if (err == nil) != c.ok {
			t.Errorf("%s：起得来应为 %v，实际错误 %v", label, c.ok, err)
		}
	}
}

// 调查的两个代理按 HARO_RESEARCH_* 发请求：思考写法、每次输出上限、力度两个都用；审查改用云端模型、审一次的时限按设的。
// 阳性对照：都不设时和原来一样（调查按云端模型的思考写法、定义的输出上限；审查用更强的模型和它的思考写法、定义的上限和时限）。
func TestResearchAgentsFollowResearchSettings(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`, http.StatusBadRequest)
	}))
	defer server.Close()
	requestsOf := func(settings config.ServerSettings) (research.Agents, []map[string]any) {
		t.Helper()
		mu.Lock()
		bodies = nil
		mu.Unlock()
		assembly := testAssembly(t, settings, nil)
		claude, err := llm.NewClaudeAPI("k", server.URL, server.Client())
		if err != nil {
			t.Fatal(err)
		}
		assembly.claude = claude
		built, err := assembly.researchAgents(research.DefaultLimits)
		if err != nil || built.Reviewer == nil {
			t.Fatalf("%+v %v", built, err)
		}
		request := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "要调查的问题"}}}
		for _, model := range []brain.ChatModel{built.Investigator.Model, built.Reviewer.Model} {
			if _, err := brain.CollectReply(t.Context(), model, request); err == nil {
				t.Fatal("假服务回 400：请求应失败")
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return built, slices.Clone(bodies)
	}
	describe := func(body map[string]any) string {
		thinking, _ := body["thinking"].(map[string]any)
		output, _ := body["output_config"].(map[string]any)
		return fmt.Sprintf("%v %v %v %v", body["model"], thinking["type"], output["effort"], body["max_tokens"])
	}
	settings := cloudSettings([]string{config.StrongForResearch}, config.VisionByExpert)
	settings.CloudLLMAPI, settings.AnthropicAPIKey, settings.CloudLLMURL = config.CloudAPIAnthropic, "k", server.URL
	settings.CloudLLMThinking, settings.StrongLLMThinking, settings.ResearchEffort = "disabled", "between_tools", "medium"
	settings.ResearchReviewer = config.ReviewerStrong

	built, sent := requestsOf(settings)
	if len(sent) != 2 || describe(sent[0]) != "base-model disabled medium 16000" || describe(sent[1]) != "strong-model between_tools medium 16000" ||
		built.Reviewer.Timeout != 2*time.Minute {
		t.Errorf("没设思考写法、输出上限、审查时限时：各用各的模型平常的写法，上限和时限照代理定义：%v %v，审查时限 %v",
			describe(sent[0]), describe(sent[1]), built.Reviewer.Timeout)
	}

	settings.ResearchThinking, settings.ResearchEffort, settings.ResearchMaxOutputTokens = "adaptive", "xhigh", 16000
	settings.ResearchReviewer, settings.ResearchReviewTimeout = config.ReviewerBase, 5*time.Minute
	built, sent = requestsOf(settings)
	for i, body := range sent {
		if describe(body) != "base-model adaptive xhigh 16000" {
			t.Errorf("第 %d 个代理应按 HARO_RESEARCH_* 发：%s", i+1, describe(body))
		}
	}
	if len(sent) != 2 || built.Reviewer.Timeout != 5*time.Minute {
		t.Errorf("审查的时限应是 HARO_RESEARCH_REVIEW_TIMEOUT：%v（%d 次请求）", built.Reviewer.Timeout, len(sent))
	}
}

// recordingLedger 记下每一笔花费，其余照真的账本。
type recordingLedger struct {
	spending.Ledger
	mu      sync.Mutex
	charges []spending.Charge
}

func (l *recordingLedger) Record(ctx context.Context, charge spending.Charge) error {
	l.mu.Lock()
	l.charges = append(l.charges, charge)
	l.mu.Unlock()
	return l.Ledger.Record(ctx, charge)
}

// 调查和审查的每次请求都按调查的用途记账（审查用更强的模型，照样记在调查名下、受每天的调查额度管）；请求失败的也记。
func TestResearchAgentsChargeResearch(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest) // 不重试的错误：测试不用等重试
	}))
	defer refusing.Close()
	settings := cloudSettings([]string{config.StrongForResearch}, config.VisionByExpert)
	settings.CloudLLMURL, settings.ResearchReviewer = refusing.URL+"/v1", config.ReviewerStrong
	assembly := testAssembly(t, settings, nil)
	ledger := &recordingLedger{Ledger: assembly.ledger}
	assembly.ledger = ledger
	built, err := assembly.researchAgents(research.DefaultLimits)
	if err != nil || built.Reviewer == nil {
		t.Fatalf("%+v %v", built, err)
	}
	request := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "要核对的草稿"}}}
	for _, model := range []brain.ChatModel{built.Investigator.Model, built.Reviewer.Model} {
		if _, err := brain.CollectReply(t.Context(), model, request); err == nil {
			t.Fatal("假服务回 400：请求应失败")
		}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.charges) != 2 {
		t.Fatalf("两次请求应各记一笔：%+v", ledger.charges)
	}
	for i, item := range []string{"llm:base-model", "llm:strong-model"} {
		if charge := ledger.charges[i]; charge.Purpose != spending.PurposeResearch || charge.Item != item {
			t.Errorf("第 %d 笔应按调查的用途记 %s：%+v", i+1, item, charge)
		}
	}
}

// fakeWebSearcher 是假的联网搜索（tools.WebSearcher）：交回固定的结果，记下收到的计量。
type fakeWebSearcher struct {
	recorder tools.SearchRecorder
}

func (f *fakeWebSearcher) Search(ctx context.Context, _ string, _ int, record tools.SearchRecorder) ([]tools.SearchResult, error) {
	f.recorder = record
	return []tools.SearchResult{{Title: "标题", URL: "https://example.com/a", Snippet: "摘要"}}, record.RecordSearch(ctx, 1, false)
}

func (f *fakeWebSearcher) ReadPages(_ context.Context, urls []string, _ int, _ string, _ tools.SearchRecorder) ([]tools.PageContent, error) {
	return []tools.PageContent{{URL: urls[0], Content: "正文"}, {URL: "https://example.com/bad", Error: "404"}}, nil
}

func (f *fakeWebSearcher) SearchCredits() int        { return 1 }
func (f *fakeWebSearcher) ReadCredits(pages int) int { return pages }

type countingRecorder struct{ credits int }

func (r *countingRecorder) RecordSearch(_ context.Context, credits int, _ bool) error {
	r.credits += credits
	return nil
}

// 联网搜索接成调查要的样子：结果原样换成调查的类型，记账照样经搜索那一层记，积分照转。
func TestResearchSearchAdaptsWebSearcher(t *testing.T) {
	web := &fakeWebSearcher{}
	adapter := researchSearch{searcher: web}
	record := &countingRecorder{}
	hits, err := adapter.Search(t.Context(), "x", 5, record)
	if err != nil || !slices.Equal(hits, []research.SearchHit{{Title: "标题", URL: "https://example.com/a", Snippet: "摘要"}}) ||
		record.credits != 1 {
		t.Errorf("搜索：%+v %v（记了 %d 积分）", hits, err, record.credits)
	}
	pages, err := adapter.ReadPages(t.Context(), []string{"https://example.com/a"}, 2000, "", record)
	if err != nil || !slices.Equal(pages, []research.Page{{URL: "https://example.com/a", Content: "正文"},
		{URL: "https://example.com/bad", Error: "404"}}) {
		t.Errorf("读网页：%+v %v", pages, err)
	}
	if adapter.SearchCredits() != 1 || adapter.ReadCredits(3) != 3 {
		t.Error("积分照转")
	}
}
