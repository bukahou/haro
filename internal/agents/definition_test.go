package agents_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/agents"
)

// 程序里带的代理都读得出来，调用方式、模型档、用途和程序用它们的方式一致。
func TestBuiltinDefinitions(t *testing.T) {
	catalog, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		mode           agents.Mode
		model, purpose string
	}{
		agents.Front:          {agents.ModePrimary, agents.TierBase, "chat"},
		agents.Strong:         {agents.ModeHandoff, agents.TierStrong, "strong"},
		agents.Vision:         {agents.ModeSubtask, agents.TierStrong, "vision"},
		agents.Summary:        {agents.ModeSubtask, agents.TierBase, "summary"},
		agents.OverBudget:     {agents.ModeSubtask, agents.TierBase, "chat"},
		agents.Research:       {agents.ModeSubtask, agents.TierBase, "research"},
		agents.ResearchReview: {agents.ModeSubtask, agents.TierBase, "research"}, // 审查和调查记在一起，受每天的调查额度管；用哪档由配置换
		agents.Title:          {agents.ModeSubtask, agents.TierBase, "summary"},  // 起标题也是压缩对话，记在「对话摘要」里
		agents.Backstory:      {agents.ModeSubtask, agents.TierBase, "summary"},  // 背景记录池也是压缩对话
		agents.Receipt:        {agents.ModeSubtask, agents.TierBase, "vision"},   // 记账是看图
	}
	if len(catalog) != len(want) {
		t.Errorf("程序里带了 %d 个代理，这里核对了 %d 个", len(catalog), len(want))
	}
	for name, w := range want {
		definition, err := catalog.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if definition.Mode != w.mode || definition.Model != w.model || definition.Purpose != w.purpose || !definition.Builtin {
			t.Errorf("%s：%+v", name, definition)
		}
	}
	strong, _ := catalog.Get(agents.Strong)
	if strong.Prompt != "" {
		t.Errorf("接手的代理正文应为空：%q", strong.Prompt)
	}
	review, _ := catalog.Get(agents.ResearchReview)
	if review.MaxSteps != 1 || review.Timeout <= 0 || review.MaxOutputTokens <= 1000 {
		t.Errorf("调查审查只审一次、要有时限、输出上限要放得下思考和疑点：%+v", review)
	}
}

// 调查的时限、搜索次数在配置里（HARO_RESEARCH_*），定义里写了不会生效：起不来。审查要写时限。阳性对照：去掉那一项就读得进来。
func TestResearchDefinitionsFields(t *testing.T) {
	const research = "---\ntitle: 调查\nmode: subtask\nmodel: base\npurpose: research\nmax_steps: 10\n%s---\n查资料。\n"
	const review = "---\ntitle: 调查审查\nmode: subtask\nmodel: strong\npurpose: research\nmax_steps: 1\n%s---\n核对草稿。\n"
	cases := map[string]struct {
		name, text, want string
	}{
		"调查写了时限":   {agents.Research, strings.Replace(research, "%s", "timeout: 30s\n", 1), "不认 timeout"},
		"调查写了搜索次数": {agents.Research, strings.Replace(research, "%s", "max_searches: 3\n", 1), "不认 max_searches"},
		"审查没写时限":   {agents.ResearchReview, strings.Replace(review, "%s", "", 1), "要写 timeout"},
		"调查原样":     {agents.Research, strings.Replace(research, "%s", "", 1), ""},
		"审查写了时限":   {agents.ResearchReview, strings.Replace(review, "%s", "timeout: 60s\n", 1), ""},
	}
	for label, c := range cases {
		dir := t.TempDir()
		writeAgent(t, dir, c.name, c.text)
		_, err := agents.Load(dir)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s：应读得进来：%v", label, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s：应读不进来、报错里有 %q：%v", label, c.want, err)
		}
	}
}

// 正文里引用的值由代码给；少给了就报错，不静默留空。
func TestRenderPromptNeedsEveryValue(t *testing.T) {
	catalog, err := agents.Load("")
	if err != nil {
		t.Fatal(err)
	}
	front, _ := catalog.Get(agents.Front)
	prompt, err := front.RenderPrompt(map[string]any{"EmotionTags": "[开心] [难过]", "Medium": "voice"})
	if err != nil || !strings.Contains(prompt, "只能用这几个：[开心] [难过]") || strings.Contains(prompt, "{{") {
		t.Fatalf("应填好情绪标记：%v\n%s", err, prompt)
	}
	// 打字的：说话格式换成写给人看的（能用 Markdown、不用情绪标记），语音识别那一条不要
	text, err := front.RenderPrompt(map[string]any{"EmotionTags": "[开心] [难过]", "Medium": "text"})
	if err != nil || !strings.Contains(text, "Markdown（") || strings.Contains(text, "[开心]") || strings.Contains(text, "语音识别") {
		t.Errorf("打字用的规则：%v\n%s", err, text)
	}
	if _, err := front.RenderPrompt(nil); err == nil {
		t.Error("没给 EmotionTags 时应报错")
	}
	summary, _ := catalog.Get(agents.Summary)
	if prompt, err := summary.RenderPrompt(map[string]any{"SummaryChars": 12345}); err != nil || !strings.Contains(prompt, "12345 字") {
		t.Errorf("摘要的字数上限应由代码给：%v\n%s", err, prompt)
	}
}

func writeAgent(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

const summaryOverride = "---\ntitle: 我的摘要\nmode: subtask\nmodel: base\npurpose: summary\nmax_steps: 1\ntimeout: 30s\n---\n只写一句话的摘要。\n"

// 覆盖目录里同名的文件替换程序里带的那份；其余照旧。
func TestOverrideDirectoryReplacesSameName(t *testing.T) {
	dir := t.TempDir()
	writeAgent(t, dir, agents.Summary, summaryOverride)
	catalog, err := agents.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	summary, _ := catalog.Get(agents.Summary)
	if summary.Title != "我的摘要" || summary.Prompt != "只写一句话的摘要。" || summary.Builtin {
		t.Errorf("应换成目录里的那份：%+v", summary)
	}
	if front, _ := catalog.Get(agents.Front); !front.Builtin {
		t.Errorf("没覆盖的应照旧：%+v", front)
	}
}

// 是不是程序里带的按读的来源记下，不从来源的写法推：覆盖目录用相对路径、恰好也叫 builtin 时，那里的定义照样算覆盖的。
func TestBuiltinIsRecordedNotGuessedFromSource(t *testing.T) {
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "builtin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgent(t, filepath.Join(work, "builtin"), agents.Summary, summaryOverride)
	t.Chdir(work)
	catalog, err := agents.Load("builtin")
	if err != nil {
		t.Fatal(err)
	}
	if summary, _ := catalog.Get(agents.Summary); summary.Builtin || summary.Title != "我的摘要" {
		t.Errorf("覆盖目录（相对路径 builtin）里的定义不该算程序里带的：Builtin=%v %s", summary.Builtin, summary.Source)
	}
	if front, _ := catalog.Get(agents.Front); !front.Builtin {
		t.Errorf("没覆盖的仍是程序里带的：%s", front.Source)
	}
}

// 写错了就起不来：不认识的字段、取值不对、程序不认识的代理、目录不存在。报错要说清是哪一处（want 是报错里该有的字）。
func TestInvalidDefinitionsFailToLoad(t *testing.T) {
	cases := map[string]struct{ name, text, want string }{
		"拼错的可选字段":   {agents.Summary, strings.Replace(summaryOverride, "max_steps: 1", "max_steps: 1\nmax_output_token: 500", 1), "max_output_token"},
		"模型档不对":     {agents.Summary, strings.Replace(summaryOverride, "model: base", "model: claude-haiku-5-5", 1), "model"},
		"调用方式和程序不符": {agents.Summary, strings.Replace(summaryOverride, "mode: subtask", "mode: primary", 1), "mode"},
		"步数为零":      {agents.Summary, strings.Replace(summaryOverride, "max_steps: 1", "max_steps: 0", 1), "max_steps"},
		"死循环窗口太小":   {agents.Summary, strings.Replace(summaryOverride, "max_steps: 1", "max_steps: 1\nrepeat_limit: 3\nrepeat_window: 2", 1), "repeat_window"},
		"没有用途":      {agents.Summary, strings.Replace(summaryOverride, "purpose: summary\n", "", 1), "purpose"},
		"正文为空":      {agents.Summary, strings.Replace(summaryOverride, "只写一句话的摘要。\n", "", 1), "正文"},
		"接手的代理有正文":  {agents.Strong, "---\ntitle: 强\nmode: handoff\nmodel: strong\npurpose: strong\nmax_steps: 8\n---\n多余的正文\n", "正文要留空"},
		"重复的工具":     {agents.Summary, strings.Replace(summaryOverride, "max_steps: 1", "max_steps: 1\ntools: [a, a]", 1), "重复"},
		"没有配置":      {agents.Summary, "只有正文\n", "---"},
		"写了不认的配置项":  {agents.Summary, strings.Replace(summaryOverride, "max_steps: 1", "max_steps: 1\nmax_searches: 2", 1), "不认 max_searches"},
		"该写的没写":     {agents.Summary, strings.Replace(summaryOverride, "timeout: 30s\n", "", 1), "要写 timeout"},
		"时长写错":      {agents.Summary, strings.Replace(summaryOverride, "timeout: 30s", "timeout: 30秒", 1), "timeout"},
		"配置没结束":     {agents.Summary, "---\ntitle: 摘要\n", "---"},
		"正文模板写错":    {agents.Summary, strings.Replace(summaryOverride, "只写一句话的摘要。", "{{.Broken", 1), "正文"},
		"程序不认识的代理":  {"fornt", summaryOverride, "不认识叫 fornt 的代理"},
	}
	for label, c := range cases {
		dir := t.TempDir()
		writeAgent(t, dir, c.name, c.text)
		_, err := agents.Load(dir)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应读不进来、报错里有 %q：%v", label, c.want, err)
		}
	}
	if _, err := agents.Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("配了目录却不存在时应读不进来")
	}
}

// 阳性对照：上面那组「写错」的文件只改了一处；原样的那份读得进来。
func TestValidOverrideLoads(t *testing.T) {
	dir := t.TempDir()
	writeAgent(t, dir, agents.Summary, strings.Replace(summaryOverride, "max_steps: 1", "max_steps: 1\nrepeat_limit: 3\nrepeat_window: 4\ntools: [a]", 1))
	writeAgent(t, dir, agents.Strong, "---\ntitle: 强\nmode: handoff\nmodel: strong\npurpose: strong\nmax_steps: 8\n---\n")
	if _, err := agents.Load(dir); err != nil {
		t.Fatal(err)
	}
}
