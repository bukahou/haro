package llm

import (
	"context"
	"errors"
	"fmt"
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
	primary := &scriptedModel{err: fmt.Errorf("断网：%w", brain.ErrModelUnavailable)}
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

// 冷却过后放一个请求去试：还是连不上就再冷却一段（这期间不再试）；试探的请求被调用方中途不要了也算主模型通着，
// 名额照样还回去（不然一直半开，再也不试主模型）。
func TestFallbackTrialAfterCooldown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	input := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}}
	primary := &scriptedModel{err: fmt.Errorf("断网：%w", brain.ErrModelUnavailable)}
	secondary := &scriptedModel{deltas: []brain.ReplyDelta{{Text: "备用"}}}
	model := NewFallbackChatModel(primary, "strong", secondary, "cloud", 50*time.Millisecond, logger)

	collect(t, model.StreamReply(context.Background(), input)) // 连不上 → 冷却
	time.Sleep(80 * time.Millisecond)
	collect(t, model.StreamReply(context.Background(), input)) // 过了冷却去试：还是连不上 → 再冷却
	collect(t, model.StreamReply(context.Background(), input)) // 冷却中：不试
	if primary.calls != 2 || secondary.calls != 3 {
		t.Fatalf("试探没通应再冷却：主模型 %d 次、备用 %d 次", primary.calls, secondary.calls)
	}

	time.Sleep(80 * time.Millisecond)
	primary.err, primary.deltas = nil, []brain.ReplyDelta{{Text: "一"}, {Text: "二"}}
	for range model.StreamReply(context.Background(), input) {
		break // 试探的请求：调用方拿到第一段就不要了
	}
	if text, _, _, _ := collect(t, model.StreamReply(context.Background(), input)); text != "一二" || primary.calls != 4 {
		t.Errorf("试探被中途放弃后应照常用主模型：%q（主模型被试了 %d 次）", text, primary.calls)
	}
}

// 主模型不是连不上、而是这一次失败了（拒答、请求被拒）：这一次改用备用模型，但不进冷却，下一次照常先试主模型。
func TestFallbackCoolsDownOnlyWhenUnavailable(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	input := brain.ChatRequest{Messages: []brain.ChatMessage{{Role: brain.RoleUser, Text: "hi"}}}
	primary := &scriptedModel{err: &RefusalError{Category: "cyber"}}
	secondary := &scriptedModel{deltas: []brain.ReplyDelta{{Text: "前台"}}}
	model := NewFallbackChatModel(primary, "strong", secondary, "cloud", time.Hour, logger)

	if text, _, _, err := collect(t, model.StreamReply(context.Background(), input)); err != nil || text != "前台" {
		t.Fatalf("这一次应改用备用模型：text=%q err=%v", text, err)
	}
	collect(t, model.StreamReply(context.Background(), input))
	if primary.calls != 2 {
		t.Errorf("拒答不该让主模型进冷却：主模型被试了 %d 次", primary.calls)
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
