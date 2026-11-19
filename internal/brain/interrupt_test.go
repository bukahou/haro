package brain_test

import (
	"context"
	"iter"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brain/braintest"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

// oneSecondSynth 每句产出 1 秒的声音（16 kHz 单声道 = 32000 字节），方便按时间截断。
type oneSecondSynth struct{}

func (oneSecondSynth) Synthesize(_ context.Context, _ brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		yield(realtimeproto.PCMAudio{Format: testFormat, Data: make([]byte, 32000)}, nil)
	}
}

func newInterruptTalker(t *testing.T, model brain.ChatModel) *talker {
	t.Helper()
	return newTalker(t, brain.Components{Model: model, Synthesizer: oneSecondSynth{},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(0, 40) }},
		brain.Options{MaxHistoryTurn: 6})
}

// lastAnswer 再问一轮，从模型收到的历史里取上一轮的回答。
func lastAnswer(k *talker, model *fakeModel) string {
	k.t.Helper()
	k.ask("讲个故事")
	if len(model.history) == 0 {
		k.t.Fatal("历史是空的")
	}
	return model.history[len(model.history)-1].Text
}

// 回答还在生成时被打断：截断点之前开始播的句子算听到（听到一半的也算），之后的不记。
func TestInterruptWhileGenerating(t *testing.T) {
	model := &fakeModel{tokens: []string{"从前有座山。", "山里有座庙。", "庙里有个老和尚。"}, delay: 300 * time.Millisecond}
	k := newInterruptTalker(t, model)
	go func() { // 第二句发出后（约 0.6 s）用户打断：只听到了 1.5 秒
		time.Sleep(750 * time.Millisecond)
		_ = k.session.Send(context.Background(), realtimeproto.ConversationItemTruncate{AudioEndMs: 1500})
		_ = k.session.Send(context.Background(), realtimeproto.ResponseCancel{})
	}()
	if done := braintest.Done(t, k.ask("讲个故事")); done.Status != realtimeproto.StatusCancelled {
		t.Fatalf("应被取消：%+v", done)
	}
	model.tokens, model.delay = []string{"好。"}, 0
	if got := lastAnswer(k, model); got != "从前有座山。山里有座庙。" {
		t.Errorf("历史应只记听到的两句（第二句从 1.0 s 开始播，1.5 s 时被打断），得到 %q", got)
	}
}

// 回答已经生成完、声音还在终端播放时被打断：回头把历史里这一轮的回答改成听到的部分。
func TestInterruptAfterGenerated(t *testing.T) {
	model := &fakeModel{tokens: []string{"从前有座山。", "山里有座庙。", "庙里有个老和尚。"}}
	k := newInterruptTalker(t, model)
	k.ask("讲个故事")
	k.send(realtimeproto.ConversationItemTruncate{AudioEndMs: 400})
	model.tokens = []string{"好。"}
	if got := lastAnswer(k, model); got != "从前有座山。" {
		t.Errorf("历史应改为只记第一句，得到 %q", got)
	}
}

// 还没说出一个字就被打断：历史里记一句说明，不留空。
func TestInterruptBeforeSpeaking(t *testing.T) {
	model := &fakeModel{tokens: []string{"从前有座山。"}, delay: time.Second}
	k := newInterruptTalker(t, model)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = k.session.Send(context.Background(), realtimeproto.ConversationItemTruncate{AudioEndMs: 0})
		_ = k.session.Send(context.Background(), realtimeproto.ResponseCancel{})
	}()
	k.ask("讲个故事")
	model.tokens, model.delay = []string{"好。"}, 0
	if got := lastAnswer(k, model); got != "（还没说出口就被主人打断了）" {
		t.Errorf("得到 %q", got)
	}
}
