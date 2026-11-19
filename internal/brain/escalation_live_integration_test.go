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
	"github.com/bukahou/haro/internal/realtimeproto"
)

// escalationProbe 包住真模型，记下这一轮有没有请更强的模型重答（本轮主人那句话被标上 Escalate）。
type escalationProbe struct {
	inner brain.ChatModel

	mu        sync.Mutex
	escalated bool
}

func (p *escalationProbe) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == brain.RoleUser && len(messages[i].Image) == 0 {
			p.mu.Lock()
			p.escalated = p.escalated || messages[i].Escalate
			p.mu.Unlock()
			break
		}
	}
	return p.inner.StreamReply(ctx, messages, tools)
}
func (p *escalationProbe) Warmup(ctx context.Context) error { return p.inner.Warmup(ctx) }

func (p *escalationProbe) takeEscalated() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	escalated := p.escalated
	p.escalated = false
	return escalated
}

// 真 Haiku 认不认得出「主人在纠正」（花钱很少：每种说法两轮，约 ¥0.1）：
//
//	HARO_ANTHROPIC_API_KEY=… go test -tags integration -run LiveAskStronger -v ./internal/brain/
func TestLiveAskStronger(t *testing.T) {
	key := os.Getenv("HARO_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("没设 HARO_ANTHROPIC_API_KEY")
	}
	model := llm.NewAnthropicChatModel(llm.AnthropicOptions{API: llm.NewClaudeAPI(key, "", nil), Model: "claude-haiku-5-5",
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
		probe := &escalationProbe{inner: model}
		k := newTalker(t, brain.Components{Model: probe, Synthesizer: fakeSynthesizer{}},
			brain.Options{MaxHistoryTurn: 6, Instructions: brain.DefaultInstructions, Escalation: true})
		k.send(realtimeproto.SessionUpdate{NoCamera: true}) // 不让它去看画面
		k.ask("日本的首都是哪里？")
		probe.takeEscalated()
		spoken := braintest.Spoken(k.ask(c.reply))
		got := probe.takeEscalated()
		t.Logf("%q → 交给更强的模型=%v，回答：%s", c.reply, got, spoken)
		if got != c.want {
			t.Errorf("%q：应 %v，实际 %v", c.reply, c.want, got)
		}
		_ = k.session.Close()
	}
}
