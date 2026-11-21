package brain_test

import (
	"context"
	"errors"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 模型的流结束了却没说为什么停（StopUnknown）：不当成说完，这一轮按生成失败收尾；下一轮照常回答。
func TestAnswerWithoutStopReasonIsAFailure(t *testing.T) {
	model := braintest.NewModel(t, braintest.Reply{Chunks: []string{"从前有座山，"}, NoStop: true}, braintest.Say("好的。"))
	k := newTestTalker(t, model, fakeSynthesizer{})
	if done := braintest.Done(t, k.ask("讲个故事")); done.Status != realtimeproto.StatusFailed ||
		done.Reason != realtimeproto.ReasonGenerationFailed {
		t.Errorf("没说为什么停应按生成失败收尾：%+v", done)
	}
	if spoken := k.say("然后呢"); spoken != "好的。" {
		t.Errorf("下一轮应照常回答：%q", spoken)
	}
}

// 用量不知道（模型服务不报）：回答结束的事件不带用量、这一轮的 span 不写 token 数（不编成 0）；
// 一轮里有一次请求不知道，合计就不知道。都知道时照常带上合计。
func TestUnknownUsageIsNotReported(t *testing.T) {
	ended := endedSpansSince(t)
	known, unknown := &brain.ReplyUsage{PromptTokens: 100, OutputTokens: 10}, &brain.ReplyUsage{TokensUnknown: true}
	search := brain.ToolCall{Name: "search_notes", Arguments: map[string]any{"query": "车停在哪"}}
	model := braintest.NewModel(t, braintest.Reply{ToolCalls: []brain.ToolCall{search}, Usage: known},
		braintest.Reply{Chunks: []string{"在 C3。"}, Usage: unknown}, braintest.Reply{Chunks: []string{"不客气。"}, Usage: known})
	parts := brain.Components{Front: brain.Agent{Model: model}, Synthesizer: fakeSynthesizer{}, Tools: &fakeToolbox{}}
	k := newTalker(t, parts, brain.Options{MaxHistoryTurn: 6})
	if done := braintest.Done(t, k.ask("车停哪了")); done.Status != realtimeproto.StatusCompleted || done.Usage != nil {
		t.Errorf("有一次请求不知道用量时不该带用量：%+v", done)
	}
	if done := braintest.Done(t, k.ask("谢谢")); done.Usage == nil || *done.Usage != (realtimeproto.Usage{InputTokens: 100, OutputTokens: 10}) {
		t.Errorf("都知道时应带上合计：%+v", done.Usage)
	}
	var turns []sdktrace.ReadOnlySpan
	for _, span := range ended() {
		if span.Name() == "brain.llm.stream" {
			turns = append(turns, span)
		}
	}
	if len(turns) != 2 {
		t.Fatalf("应有两轮回答的 span：%d", len(turns))
	}
	if attrs := attributesOf(turns[0]); attrs["gen_ai.usage.input_tokens"] != "" || attrs["gen_ai.usage.output_tokens"] != "" {
		t.Errorf("不知道时不该写 token 数：%v", attrs)
	}
	if attrs := attributesOf(turns[1]); attrs["gen_ai.usage.input_tokens"] != "100" || attrs["gen_ai.usage.output_tokens"] != "10" {
		t.Errorf("知道时应写 token 数：%v", attrs)
	}
}

// 收齐回答（摘要、调查、看画面的专家用）：说完了照常交出；被截断交出 ErrTruncated；流结束了却没说为什么停
// 交出 ErrStopUnknown（不当成说完）。收到的文字都带回来。
func TestCollectReplyNeedsAStopReason(t *testing.T) {
	request := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "写个摘要"}}}
	for _, c := range []struct {
		name  string
		reply braintest.Reply
		want  error
	}{
		{"说完了", braintest.Reply{Chunks: []string{"主人的车停在 B2。"}}, nil},
		{"被截断", braintest.Reply{Chunks: []string{"主人的车停在 B2。"}, Truncated: true}, brain.ErrTruncated},
		{"没说为什么停", braintest.Reply{Chunks: []string{"主人的车停在 B2。"}, NoStop: true}, brain.ErrStopUnknown},
	} {
		reply, err := brain.CollectReply(context.Background(), braintest.NewModel(t, c.reply), request)
		if !errors.Is(err, c.want) || reply.Text != "主人的车停在 B2。" {
			t.Errorf("%s：应交出 %v，实际 %v（%q）", c.name, c.want, err, reply.Text)
		}
	}
}
