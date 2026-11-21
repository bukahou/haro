//go:build integration

package brain_test

import (
	"context"
	"iter"
	"os"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/llm"
)

// handoffProbe 包住接手的代理用的真模型，记下这一轮有没有交给它（被调用过就是交出去了）。
type handoffProbe struct {
	inner brain.ChatModel

	mu     sync.Mutex
	called bool
}

func (p *handoffProbe) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	p.mu.Lock()
	p.called = true
	p.mu.Unlock()
	return p.inner.StreamReply(ctx, request)
}

func (p *handoffProbe) takeCalled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	called := p.called
	p.called = false
	return called
}

// 真 Haiku 认不认得出「主人在纠正」（花钱很少：每种说法两轮，约 ¥0.1）：
//
//	HARO_ANTHROPIC_API_KEY=… go test -tags integration -run LiveAskStronger -v ./internal/brain/
func TestLiveAskStronger(t *testing.T) {
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY")
	}
	api, err := llm.NewClaudeAPI(key, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	model := llm.NewAnthropicChatModel(llm.AnthropicOptions{API: api, Model: "claude-haiku-5-5",
		MaxTokens: 400, Effort: "low", Thinking: llm.ThinkingDisabled})
	cases := []struct {
		reply string
		want  bool
	}{
		{"不对，你再想想", true},
		{"你说错了吧", true},
		{"違うよ、もう一度考えて", true},
		{"No, that's wrong.", true},
		{"这个问题你想清楚一点再回答", true},
		{"好的，谢谢", false},
		{"那北海道的首府呢？", false},
	}
	for _, c := range cases {
		probe := &handoffProbe{inner: model} // 接手的也用 Haiku：这里只看前台交不交出去
		k := newTalker(t, brain.Components{Front: brain.Agent{Model: model, Instructions: builtinFrontInstructions(t)},
			Handoff: brain.Handoff{Agent: strongAgent(probe, 8), OnCorrection: true}, Synthesizer: fakeSynthesizer{}},
			brain.Options{MaxHistoryTurn: 6})
		k.ask("日本的首都是哪里？")
		probe.takeCalled()
		spoken := braintest.Spoken(k.ask(c.reply))
		got := probe.takeCalled()
		t.Logf("%q → 交给更强的模型=%v，回答：%s", c.reply, got, spoken)
		if got != c.want {
			t.Errorf("%q：应 %v，实际 %v", c.reply, c.want, got)
		}
		_ = k.session.Close()
	}
}
