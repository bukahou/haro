package brain_test

import (
	"slices"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
)

// sayWithContext 是回答一句、报这次请求带了 promptTokens 的上下文。
func sayWithContext(text string, promptTokens int) braintest.Reply {
	return braintest.Reply{Chunks: []string{text}, Usage: &brain.ReplyUsage{PromptTokens: promptTokens, OutputTokens: 5, Requests: 1}}
}

// userTexts 是这次请求里主人说过的话（不含系统合成的）。
func userTexts(request braintest.Request) []string {
	var texts []string
	for _, msg := range request.Messages {
		if msg.Role == brain.RoleUser && !msg.Synthetic {
			texts = append(texts, msg.Text)
		}
	}
	return texts
}

// 一轮里请求模型带的上下文（最大的那次请求的输入 token）到了门槛：较早的几轮提前移出窗口，下一轮只带最近两轮。
func TestLargeContextCompactsHistory(t *testing.T) {
	model := braintest.NewModel(t, sayWithContext("一", 100), sayWithContext("二", 100), sayWithContext("三", 100),
		sayWithContext("四", 5000), sayWithContext("五", 100))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}},
		brain.Options{MaxHistoryTurn: 6, CompactContextTokens: 4000})
	for _, text := range []string{"第一句", "第二句", "第三句", "第四句", "第五句"} {
		k.say(text)
	}
	if got := userTexts(model.LastRequest()); !slices.Equal(got, []string{"第三句", "第四句", "第五句"}) {
		t.Errorf("上下文到了门槛之后只带最近两轮：%v", got)
	}
}

// 门槛看的是一次请求带了多少（多步调用工具时每次都重带一遍上下文），不是一轮里几次请求加起来。
func TestCompactionLooksAtTheLargestRequest(t *testing.T) {
	first := braintest.Reply{ToolCalls: searchFor("车").ToolCalls, Usage: &brain.ReplyUsage{PromptTokens: 3000, Requests: 1}}
	model := braintest.NewModel(t, sayWithContext("一", 100), sayWithContext("二", 100), sayWithContext("三", 100),
		first, sayWithContext("在 B2。", 4500), sayWithContext("好。", 100))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Tools: &fakeToolbox{}},
		brain.Options{MaxHistoryTurn: 6, CompactContextTokens: 5000})
	texts := []string{"第一句", "第二句", "第三句", "车停在哪", "谢谢"}
	for _, text := range texts {
		k.say(text)
	}
	if got := userTexts(model.LastRequest()); !slices.Equal(got, texts) {
		t.Errorf("最大的一次 4500 没到 5000（加起来 7500 到了）：不该压缩：%v", got)
	}
}

// 没设门槛时照旧（只按 MaxHistoryTurn）。
func TestNoCompactionWithoutThreshold(t *testing.T) {
	model := braintest.NewModel(t, sayWithContext("一", 100), sayWithContext("二", 90000), sayWithContext("三", 100))
	k := newTalker(t, brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}}, brain.Options{MaxHistoryTurn: 6})
	for _, text := range []string{"第一句", "第二句", "第三句"} {
		k.say(text)
	}
	if got := userTexts(model.LastRequest()); !slices.Equal(got, []string{"第一句", "第二句", "第三句"}) {
		t.Errorf("没设门槛时不压缩：%v", got)
	}
}
