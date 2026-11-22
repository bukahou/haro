package research

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/brain"
)

// reviewerModel 是假的审查模型：交回测试指定的那一次回答（reply），或出错（err）、一直等到被取消（hang）；记下收到的请求。
type reviewerModel struct {
	mu       sync.Mutex
	reply    brain.ReplyDelta
	err      error
	hang     bool
	requests []brain.ChatRequest
}

func (m *reviewerModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	m.mu.Lock()
	m.requests = append(m.requests, request)
	reply, err, hang := m.reply, m.err, m.hang
	m.mu.Unlock()
	return func(yield func(brain.ReplyDelta, error) bool) {
		if hang {
			<-ctx.Done()
			yield(brain.ReplyDelta{}, ctx.Err())
			return
		}
		if err != nil {
			yield(brain.ReplyDelta{}, err)
			return
		}
		reply.Usage, reply.Stop = &brain.ReplyUsage{PromptTokens: 3000, OutputTokens: 500}, brain.StopCompleted
		yield(reply, nil)
	}
}

// seen 是审查收到的请求。
func (m *reviewerModel) seen() []brain.ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

// material 解出审查收到的那份内容（第二条消息）。
func (m *reviewerModel) material(t *testing.T) reviewMaterial {
	t.Helper()
	requests := m.seen()
	if len(requests) != 1 || len(requests[0].Messages) != 2 {
		t.Fatalf("审查应只请求一次，只带系统提示词和一条内容：%d 次 %+v", len(requests), requests)
	}
	var material reviewMaterial
	if err := json.Unmarshal([]byte(requests[0].Messages[1].Text), &material); err != nil {
		t.Fatalf("交给审查的内容应是 JSON：%v", err)
	}
	return material
}

// reviewPass、reviewDoubts 是审查用 submit_review 交回的两种结论。
func reviewPass() brain.ReplyDelta { return call("submit_review", map[string]any{"verdict": "pass"}) }

func reviewDoubts(doubts ...map[string]any) brain.ReplyDelta {
	return call("submit_review", map[string]any{"verdict": "doubts", "doubts": doubts})
}

func doubt(claim, kind, evidence string) map[string]any {
	return map[string]any{"claim": claim, "kind": kind, "evidence": evidence}
}

const revisedConclusion = "复查之后：A 比 B 便宜，B 的价格没查到。"

// 草稿交给审查，审查交「通过」：用草稿，不复查。
func TestReviewPassKeepsDraft(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A B 价格"}),
		submit(scriptedConclusion, "https://example.com/a")}}
	reviewer := &reviewerModel{reply: reviewPass()}
	done := newRig(t, model, rigSetup{reviewer: reviewer}).research(t, "A 和 B 哪个便宜")
	if done.Status != StatusDone || done.Conclusion != scriptedConclusion || done.StopReason != "" {
		t.Errorf("审查通过：应用草稿：%+v", done)
	}
	if material := reviewer.material(t); material.Draft != scriptedConclusion {
		t.Errorf("审查的应是草稿：%+v", material)
	}
	if model.requests != 2 {
		t.Errorf("通过了就不复查：调查的模型被问了 %d 次", model.requests)
	}
}

// 审查交疑点：疑点作为一条消息交回原来的调查对话（之前的对话原样留着），调查的模型复查（再搜）、再交：用新结论。
// 代码不改结论的文字：交出去的就是复查后交的那份。
func TestReviewDoubtsAreRecheckedInTheSameConversation(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{
		call("search_web", map[string]any{"query": "A B 价格"}),
		submit(scriptedConclusion, "https://example.com/a"),
		call("search_web", map[string]any{"query": "B 的价格"}),
		submit(revisedConclusion, "https://example.com/a", "https://example.com/b"),
	}}
	reviewer := &reviewerModel{reply: reviewDoubts(doubt("B 更贵", "unsupported", "摘录里只有 A 的价格"))}
	rig := newRig(t, model, rigSetup{reviewer: reviewer})
	done := rig.research(t, "A 和 B 哪个便宜")
	if done.Status != StatusDone || done.Conclusion != revisedConclusion ||
		!slices.Equal(done.Sources, []string{"https://example.com/a", "https://example.com/b"}) || done.StopReason != "" {
		t.Errorf("复查后交了新结论：应用新结论和它的来源：%+v", done)
	}
	if len(reviewer.seen()) != 1 {
		t.Errorf("只审一次：审了 %d 次", len(reviewer.seen()))
	}
	requests := model.requestMessages()
	if len(requests) != 4 {
		t.Fatalf("调查的模型应被问 4 次（搜、交草稿、复查时搜、再交）：%d 次", len(requests))
	}
	draftStep, recheck := requests[1], requests[2]
	if !slices.EqualFunc(recheck[:len(draftStep)], draftStep, sameMessage) || recheck[0].Text != testPrompt {
		t.Error("复查接着原来的调查对话：之前的消息原样留着、系统提示词不变")
	}
	tail := recheck[len(draftStep):] // 交草稿那一步之后加的：模型交草稿的调用、它的结果、疑点
	if len(tail) != 3 || tail[0].Role != brain.RoleAssistant || tail[1].Role != brain.RoleTool ||
		tail[1].ToolCallID != tail[0].ToolCalls[0].ID || tail[2].Role != brain.RoleUser {
		t.Fatalf("交草稿的调用要先回结果，再交疑点：%+v", tail)
	}
	var sent struct {
		Doubts []recheckDoubt `json:"doubts"`
	}
	payload := tail[2].Text[strings.Index(tail[2].Text, "{"):]
	if err := json.Unmarshal([]byte(payload), &sent); err != nil ||
		!slices.Equal(sent.Doubts, []recheckDoubt{{Claim: "B 更贵", Problem: "没依据", Evidence: "摘录里只有 A 的价格"}}) {
		t.Errorf("疑点照审查交回的原样交给调查的模型（问题种类换成说法）：%+v %v", sent, err)
	}
	if searches, _ := rig.searcher.counts(); searches != 2 {
		t.Errorf("复查时搜索照常执行：搜了 %d 次", searches)
	}
}

func sameMessage(a, b brain.ChatMessage) bool {
	return a.Role == b.Role && a.Text == b.Text && a.ToolCallID == b.ToolCallID && len(a.ToolCalls) == len(b.ToolCalls)
}

// 审查没做成（出错、超时、没用工具交回、交回的不对、额度用完）：用草稿，原因里写明没审查成，不复查。
func TestReviewFailureKeepsDraft(t *testing.T) {
	overloaded := errors.New("overloaded")
	cases := []struct {
		name     string
		reviewer *reviewerModel
		refuse   bool          // 审查前调查的额度用完了
		timeout  time.Duration // 审查的时限
		reason   string        // 原因里该有的
	}{
		{name: "审查出错", reviewer: &reviewerModel{err: overloaded}, reason: overloaded.Error()},
		{name: "审查超时", reviewer: &reviewerModel{hang: true}, timeout: 50 * time.Millisecond, reason: "超过了"},
		{name: "没用工具交回", reviewer: &reviewerModel{reply: brain.ReplyDelta{Text: "都有依据。"}}, reason: "submit_review"},
		{name: "问题种类不认识", reviewer: &reviewerModel{reply: reviewDoubts(doubt("B 更贵", "made_up", "x"))}, reason: "made_up"},
		{name: "通过却列了疑点", reviewer: &reviewerModel{reply: call("submit_review", map[string]any{"verdict": "pass",
			"doubts": []map[string]any{doubt("B 更贵", "unsupported", "x")}})}, reason: "pass"},
		{name: "有疑点却没列", reviewer: &reviewerModel{reply: call("submit_review", map[string]any{"verdict": "doubts"})}, reason: "doubts"},
		{name: "缺了必填的参数", reviewer: &reviewerModel{reply: call("submit_review", map[string]any{})}, reason: "verdict"},
		{name: "额度用完", reviewer: &reviewerModel{reply: reviewPass()}, refuse: true, reason: "额度"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A"}),
				submit(scriptedConclusion, "https://example.com/a")}}
			rig := newRig(t, model, rigSetup{reviewer: c.reviewer, reviewTimeout: c.timeout})
			if c.refuse { // 交草稿的那一步开始时额度用完：审查前查额度不过
				model.onRequest = map[int]func(){2: func() { rig.budget.refuseWith(errors.New("今天的调查额度用完了")) }}
			}
			done := rig.research(t, "要查的事")
			if done.Status != StatusDone || done.Conclusion != scriptedConclusion || !strings.Contains(done.StopReason, c.reason) {
				t.Errorf("应用草稿、原因里写明没审查成（含 %q）：%+v", c.reason, done)
			}
			if model.requests != 2 {
				t.Errorf("没审查成就不复查：调查的模型被问了 %d 次", model.requests)
			}
			if wantCalls := !c.refuse; (len(c.reviewer.seen()) == 1) != wantCalls {
				t.Errorf("额度用完时不请求审查，其余只请求一次：%d 次", len(c.reviewer.seen()))
			}
		})
	}
}

// 有疑点、复查没交出新结论（模型出错、轮数用完）：用草稿，记成「部分」，原因里写明审查发现了疑点、没能复查。
// 复查只多给 recheckSearches 次搜索：用完后再搜不执行。
func TestRecheckWithoutNewConclusionKeepsDraftAsPartial(t *testing.T) {
	keepSearching := call("search_web", map[string]any{"query": "还在找"})
	cases := []struct {
		name     string
		model    *scriptedModel
		searches int // 一共搜了几次
	}{
		{"复查时模型出错", &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A"}),
			submit(scriptedConclusion, "https://example.com/a")}, failures: map[int]error{3: errors.New("overloaded")}}, 1},
		{"复查一直在搜、轮数用完", &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A"}),
			submit(scriptedConclusion, "https://example.com/a")}, repeat: &keepSearching}, 1 + recheckSearches},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reviewer := &reviewerModel{reply: reviewDoubts(doubt("A 比 B 便宜", "contradicted", "摘录说 B 更便宜"),
				doubt("A 100 円", "inaccurate", "摘录写的是 110 円"))}
			rig := newRig(t, c.model, rigSetup{reviewer: reviewer})
			done := rig.research(t, "A 和 B 哪个便宜")
			if done.Status != StatusPartial || done.Conclusion != scriptedConclusion || !strings.Contains(done.StopReason, "2 处疑点") {
				t.Errorf("应用草稿、记成部分、写明审查发现了疑点没能复查：%+v", done)
			}
			if searches, _ := rig.searcher.counts(); searches != c.searches {
				t.Errorf("搜了 %d 次，应为 %d 次（复查最多再搜 %d 次）", searches, c.searches, recheckSearches)
			}
			if c.model.requests > 2+recheckRounds {
				t.Errorf("复查最多 %d 轮：调查的模型一共被问了 %d 次", recheckRounds, c.model.requests)
			}
		})
	}
}

// 到上限时逼出来的草稿同样交给审查（审查通过：用它，照样记成部分）。
func TestDraftWrittenAtLimitsIsReviewed(t *testing.T) {
	search := call("search_web", map[string]any{"query": "没完没了"})
	model := &scriptedModel{repeat: &search}
	reviewer := &reviewerModel{reply: reviewPass()}
	done := newRig(t, model, rigSetup{reviewer: reviewer, rounds: 2}).research(t, "无底洞")
	if done.Status != StatusPartial || done.Conclusion != limitConclusion {
		t.Errorf("到上限写的结论审查通过后照用，记成部分：%+v", done)
	}
	if material := reviewer.material(t); material.Draft != limitConclusion {
		t.Errorf("审查的应是到上限时写的草稿：%+v", material)
	}
}

// 交给审查的只有问题、草稿和草稿列出的来源的摘录：调查过程里别的资料（没列为来源的网页、搜索结果、搜索词）不给；
// 列了却没拿到内容的写明；每个摘录、总长都不超过上限。
func TestReviewSeesOnlyQuestionDraftAndCitedExcerpts(t *testing.T) {
	const (
		urlA = "https://example.com/a" // 读了正文、列为来源（正文很长）
		urlB = "https://example.com/b" // 只在搜索结果里、列为来源
		urlC = "https://example.com/c" // 读了正文、没列为来源
		urlD = "https://example.com/d" // 列为来源，但调查时没拿到
		urlE = "https://example.com/e" // 只在搜索结果里、没列为来源
	)
	model := &scriptedModel{replies: []brain.ReplyDelta{
		call("search_web", map[string]any{"query": "搜索词-不该给审查"}),
		call("read_pages", map[string]any{"urls": []any{urlA, urlC}}),
		submit(scriptedConclusion, urlA+"/", urlB, urlD, urlA), // 网址的写法略有不同、重复：按同一个来源算
	}}
	reviewer := &reviewerModel{reply: reviewPass()}
	rig := newRig(t, model, rigSetup{reviewer: reviewer})
	rig.searcher.hits = []SearchHit{{Title: "A", URL: urlA, Snippet: "A 的摘要"}, {Title: "B", URL: urlB, Snippet: "B 的摘要"},
		{Title: "E", URL: urlE, Snippet: "E 的摘要-不该给审查"}}
	rig.searcher.pages = map[string]string{urlA: strings.Repeat("甲", 2*pageRunes), urlC: "C 的正文-不该给审查"}
	const question = "A 和 B 哪个便宜"
	rig.research(t, question)

	text := reviewer.seen()[0].Messages[1].Text
	for _, leaked := range []string{"不该给审查", "read_pages", "search_web"} {
		if strings.Contains(text, leaked) {
			t.Errorf("交给审查的内容里不该有调查过程的资料 %q", leaked)
		}
	}
	material := reviewer.material(t)
	if material.Question != question || material.Draft != scriptedConclusion || material.Today == "" {
		t.Errorf("应有问题、草稿、现在的日期：%+v", material)
	}
	if len(material.Sources) != 3 {
		t.Fatalf("应只有草稿列出的三个来源（同一个只给一次）：%+v", material.Sources)
	}
	a, b, d := material.Sources[0], material.Sources[1], material.Sources[2]
	if a.From != fromPage || utf8.RuneCountInString(a.Excerpt) != maxExcerptRunes || a.Note != noteTruncation {
		t.Errorf("读了正文的来源：给正文，截到 %d 字并写明：%s %d %q", maxExcerptRunes, a.From, utf8.RuneCountInString(a.Excerpt), a.Note)
	}
	if b.From != fromSnippet || b.Excerpt != "B 的摘要" || b.Title != "B" {
		t.Errorf("只在搜索结果里的来源：给标题和摘要：%+v", b)
	}
	if d.URL != urlD || d.Excerpt != "" || d.Note != noteMissing {
		t.Errorf("没拿到内容的来源：写明调查时没有拿到：%+v", d)
	}
}

// 给审查的摘录一共不超过 maxReviewSourceRunes 字：放不下的来源写明没有附上（不当成没拿到）。
func TestReviewExcerptsHaveATotalLimit(t *testing.T) {
	urls := []string{"https://example.com/1", "https://example.com/2", "https://example.com/3", "https://example.com/4"}
	notes := sourceNotes{}
	pages := make([]Page, len(urls))
	for i, url := range urls {
		pages[i] = Page{URL: url, Content: strings.Repeat("字", maxExcerptRunes)}
	}
	notes.addPages(pages)
	excerpts := notes.excerptsFor(urls)
	total := 0
	for _, excerpt := range excerpts {
		total += utf8.RuneCountInString(excerpt.Excerpt)
	}
	if total > maxReviewSourceRunes || len(excerpts) != 4 || excerpts[3].Note != noteOmitted || excerpts[3].Excerpt != "" {
		t.Errorf("总长 %d 字（上限 %d）；放不下的应写明没附上：%+v", total, maxReviewSourceRunes, excerpts[3])
	}
}

// 网址按结构对照：协议、主机名的大小写，# 后面的部分，路径末尾的 / 不同，算同一个来源。
func TestSourceKeyIgnoresCosmeticDifferences(t *testing.T) {
	want := sourceKey("https://example.com/item/1")
	for _, variant := range []string{"HTTPS://Example.COM/item/1", "https://example.com/item/1/", "https://example.com/item/1#top",
		" https://example.com/item/1 "} {
		if got := sourceKey(variant); got != want {
			t.Errorf("%q 应和 %q 算同一个来源：%q", variant, want, got)
		}
	}
	if sourceKey("https://example.com/item/1?page=2") == want || sourceKey("https://example.com/item/2") == want {
		t.Error("查询参数、路径不同的是不同的来源")
	}
}

// 系统提示词逐字不变（现在的日期每天不同，放在任务的消息里）：隔天派的调查，系统提示词一样，任务里写着各自的日期。
func TestSystemPromptIsStableAndTodayIsInTheTask(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A"}),
		submit(scriptedConclusion), call("search_web", map[string]any{"query": "A"}), submit(scriptedConclusion)}}
	rig := newRig(t, model, rigSetup{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.run(ctx)
	days := []time.Time{time.Date(2026, 11, 17, 9, 0, 0, 0, time.Local), time.Date(2026, 11, 18, 9, 0, 0, 0, time.Local)}
	for _, day := range days { // 一个查完再派下一个：换日期时执行器没在查
		rig.runner.now = func() time.Time { return day }
		task, _, err := rig.runner.Start(ctx, "主人", "要查的事")
		if err != nil {
			t.Fatal(err)
		}
		rig.waitFinished(t, task.ID)
	}
	requests := model.requestMessages()
	first, second := requests[0], requests[2]
	if first[0].Text != testPrompt || second[0].Text != testPrompt {
		t.Errorf("系统提示词应逐字不变：%q %q", first[0].Text, second[0].Text)
	}
	for i, task := range []string{first[1].Text, second[1].Text} {
		if !strings.Contains(task, brain.DateText(days[i])) || !strings.Contains(task, "要查的事") {
			t.Errorf("任务里应写着当天的日期和问题：%q", task)
		}
	}
}

// 调查的代理的允许名单里没有的工具：不交给模型，模型照着名字调用也不执行。
func TestInvestigatorOnlyUsesAllowedTools(t *testing.T) {
	model := &scriptedModel{replies: []brain.ReplyDelta{call("search_web", map[string]any{"query": "A"}),
		call("read_pages", map[string]any{"urls": []any{"https://example.com/a"}})}}
	rig := newRig(t, model, rigSetup{tools: []string{searchWebTool, submitConclusionTool}})
	if done := rig.research(t, "要查的事"); done.Status != StatusDone {
		t.Fatalf("%+v", done)
	}
	if _, reads := rig.searcher.counts(); reads != 0 {
		t.Errorf("没交给它的 read_pages 不该执行：读了 %d 次", reads)
	}
	if offers(rig.runner.investigationSpecs, readPagesTool) || !offers(rig.runner.investigationSpecs, searchWebTool) {
		t.Errorf("交给模型的工具应按允许名单筛：%v", rig.runner.investigationSpecs)
	}
}

// 两个代理不合要求时建不起来（写了不会生效、或者根本没法工作）：调查要能搜、能交结论；审查要能交回审查、只请求一次、有时限。
// 阳性对照：合要求的建得起来。
func TestNewRunnerChecksAgents(t *testing.T) {
	store := openTestStore(t)
	valid := rigSetup{reviewer: &reviewerModel{}}.agents(&scriptedModel{})
	cases := map[string]func(*Agents){
		"调查不能交结论":  func(a *Agents) { a.Investigator.Tools = []string{searchWebTool, readPagesTool} },
		"调查不能搜":    func(a *Agents) { a.Investigator.Tools = []string{readPagesTool, submitConclusionTool} },
		"调查没有轮数":   func(a *Agents) { a.Investigator.MaxSteps = 0 },
		"调查写了时限":   func(a *Agents) { a.Investigator.Timeout = time.Minute },
		"调查写了重复检查": func(a *Agents) { a.Investigator.RepeatLimit = 3 },
		"审查不能交回": func(a *Agents) {
			reviewer := *a.Reviewer
			reviewer.Tools = []string{searchWebTool}
			a.Reviewer = &reviewer
		},
		"审查请求不止一次":  func(a *Agents) { reviewer := *a.Reviewer; reviewer.MaxSteps = 2; a.Reviewer = &reviewer },
		"审查没有时限":    func(a *Agents) { reviewer := *a.Reviewer; reviewer.Timeout = 0; a.Reviewer = &reviewer },
		"审查没有系统提示词": func(a *Agents) { reviewer := *a.Reviewer; reviewer.Instructions = ""; a.Reviewer = &reviewer },
	}
	for name, change := range cases {
		agents := valid
		change(&agents)
		if _, err := NewRunner(store, agents, &fakeSearcher{}, &fakeBudget{}, &fakeBudget{}, DefaultLimits, quietLogger); err == nil {
			t.Errorf("%s：应建不起来", name)
		}
	}
	if _, err := NewRunner(store, valid, &fakeSearcher{}, &fakeBudget{}, &fakeBudget{}, DefaultLimits, quietLogger); err != nil {
		t.Errorf("合要求的应建得起来：%v", err)
	}
	if _, err := NewRunner(store, valid, &fakeSearcher{}, &fakeBudget{}, &fakeBudget{}, Limits{}, quietLogger); err == nil {
		t.Error("上限是 0 时应建不起来")
	}
}

// 正文里的工具名只填允许名单里有的（正文提到没交给它的工具时填值会报错）；上限、复查的额度、有没有审查都由代码给。
func TestPromptValuesFollowAllowList(t *testing.T) {
	values := InvestigationPromptValues([]string{searchWebTool, submitConclusionTool}, 7, DefaultLimits, true)
	if _, ok := values["ReadTool"]; ok || values["SearchTool"] != searchWebTool || values["SubmitTool"] != submitConclusionTool {
		t.Errorf("工具名只填允许名单里有的：%v", values)
	}
	if values["MaxRounds"] != 7 || values["MaxSearches"] != DefaultLimits.MaxSearches || values["Reviewed"] != true ||
		values["RecheckSearches"] != recheckSearches || values["RecheckRounds"] != recheckRounds {
		t.Errorf("上限和复查的额度由代码给：%v", values)
	}
	if all := InvestigationPromptValues(nil, 7, DefaultLimits, false); all["ReadTool"] != readPagesTool {
		t.Errorf("没写允许名单 = 全部：%v", all)
	}
	if review := ReviewPromptValues(nil); review["ReviewTool"] != submitReviewTool {
		t.Errorf("审查的工具名：%v", review)
	}
	if review := ReviewPromptValues([]string{}); len(review) != 0 {
		t.Errorf("允许名单是空的：不填工具名：%v", review)
	}
}

// submit_review 的参数定义里，结论和问题种类是枚举（取值和交回后的核对用同一份）。
func TestReviewParametersDeclareEnums(t *testing.T) {
	var schema struct {
		Properties struct {
			Verdict struct {
				Enum []string `json:"enum"`
			} `json:"verdict"`
			Doubts struct {
				Items struct {
					Properties struct {
						Kind struct {
							Enum []string `json:"enum"`
						} `json:"kind"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"doubts"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(submitReviewSpec.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(schema.Properties.Verdict.Enum, []string{"pass", "doubts"}) ||
		!slices.Equal(schema.Properties.Doubts.Items.Properties.Kind.Enum, []string{"unsupported", "contradicted", "inaccurate"}) {
		t.Errorf("枚举不对：%s", submitReviewSpec.Parameters)
	}
}
