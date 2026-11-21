package brain_test

import (
	"context"
	"errors"
	"testing"

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
