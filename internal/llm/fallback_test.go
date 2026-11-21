package llm

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

type scriptedModel struct {
	deltas []brain.ReplyDelta
	err    error
	calls  int
}

func (s *scriptedModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	s.calls++
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, delta := range s.deltas {
			if !yield(delta, nil) {
				return
			}
		}
		if s.err != nil {
			yield(brain.ReplyDelta{}, s.err)
		}
	}
}

func TestFallbackSwitchesOnlyBeforeAnyOutput(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	input := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}}

	// 主模型一开始就失败 → 这一轮用备用模型
	primary := &scriptedModel{err: errors.New("断网")}
	secondary := &scriptedModel{deltas: []brain.ReplyDelta{{Text: "本地回答"}}}
	text, _, _, err := collect(t, NewFallbackChatModel(primary, "cloud", secondary, "local", 0, logger).StreamReply(context.Background(), input))
	if err != nil || text != "本地回答" {
		t.Errorf("应退回本地：text=%q err=%v", text, err)
	}

	// 主模型说到一半才失败 → 不切换（避免说两遍），报错
	primary = &scriptedModel{deltas: []brain.ReplyDelta{{Text: "说了一半"}}, err: errors.New("连接断了")}
	secondary = &scriptedModel{deltas: []brain.ReplyDelta{{Text: "不该出现"}}}
	text, _, _, err = collect(t, NewFallbackChatModel(primary, "cloud", secondary, "local", 0, logger).StreamReply(context.Background(), input))
	if err == nil || text != "说了一半" || secondary.calls != 0 {
		t.Errorf("中途失败不应切换：text=%q err=%v secondary.calls=%d", text, err, secondary.calls)
	}

	// 主模型正常 → 不碰备用模型
	primary = &scriptedModel{deltas: []brain.ReplyDelta{{Text: "云端回答"}}}
	secondary = &scriptedModel{}
	text, _, _, _ = collect(t, NewFallbackChatModel(primary, "cloud", secondary, "local", 0, logger).StreamReply(context.Background(), input))
	if text != "云端回答" || secondary.calls != 0 {
		t.Errorf("text=%q secondary.calls=%d", text, secondary.calls)
	}
}

func TestFallbackCircuitBreaker(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	input := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}}
	primary := &scriptedModel{err: errors.New("断网")}
	secondary := &scriptedModel{deltas: []brain.ReplyDelta{{Text: "本地"}}}
	model := NewFallbackChatModel(primary, "cloud", secondary, "local", 100*time.Millisecond, logger)

	collect(t, model.StreamReply(context.Background(), input)) // 主模型失败 → 熔断
	collect(t, model.StreamReply(context.Background(), input)) // 熔断中：不应再试主模型
	if primary.calls != 1 || secondary.calls != 2 {
		t.Fatalf("熔断中不应再试主模型：primary=%d secondary=%d", primary.calls, secondary.calls)
	}
	time.Sleep(150 * time.Millisecond) // 过了冷却期：再试一次主模型
	primary.err, primary.deltas = nil, []brain.ReplyDelta{{Text: "云端"}}
	if text, _, _, _ := collect(t, model.StreamReply(context.Background(), input)); text != "云端" || primary.calls != 2 {
		t.Errorf("冷却期后应切回主模型：text=%q primary=%d", text, primary.calls)
	}
}

// warmupModel 是需要预热的模型（brain.ModelWarmer），记下有没有被预热。
type warmupModel struct {
	scriptedModel
	warmed bool
	err    error
}

func (w *warmupModel) Warmup(context.Context) error { w.warmed = true; return w.err }

// 主备里需要预热的都预热（本地当主模型时也要）；主模型预热失败不影响备用；不需要预热的（云端）跳过。
func TestFallbackWarmsModelsThatNeedIt(t *testing.T) {
	primary, secondary := &warmupModel{err: errors.New("本地模型那台机器离线了")}, &warmupModel{}
	model := NewFallbackChatModel(primary, "local", secondary, "cloud", time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := brain.WarmUp(context.Background(), model); err == nil || !primary.warmed || !secondary.warmed {
		t.Errorf("主备都应预热、主模型的错误要报出来：err=%v primary=%v secondary=%v", err, primary.warmed, secondary.warmed)
	}
	cloudOnly := NewFallbackChatModel(&scriptedModel{}, "cloud", &warmupModel{}, "local", time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := brain.WarmUp(context.Background(), cloudOnly); err != nil {
		t.Errorf("不需要预热的模型跳过：%v", err)
	}
}
