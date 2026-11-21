package llm

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"time"

	"github.com/failsafe-go/failsafe-go/circuitbreaker"

	"github.com/bukahou/haro/internal/brain"
)

// FallbackChatModel 先用主模型；主模型在**产出任何内容之前**就失败了（断网、超时、服务报错），改用备用模型。
// 已经开始说了才失败的不切换（否则会说两遍），直接报错。现在只用在「更强的模型出错时由原来那套云端模型接手」：
// 云端不再自动换本地模型（第 7 段）。
//
// 冷却：主模型连不上（brain.ErrModelUnavailable）后的 cooldown 内直接用备用模型、不再试主模型；过了 cooldown 放一个请求
// 去试，成功就切回去（failsafe-go 的熔断器，和 UnavailabilityBreaker 同一个做法）。否则一句话里每轮推理（用工具时有两三轮）
// 都要先等主模型超时（2026-11-15 实测断网时等了 17 s）。
// 别的失败（拒答、请求被拒）只针对这一次请求：这一次改用备用模型，下一次照常先试主模型。
type FallbackChatModel struct {
	primary, secondary         brain.ChatModel
	primaryName, secondaryName string // 用于日志，例如 "strong" / "cloud"
	cooldown                   time.Duration
	logger                     *slog.Logger
	// primaryDown 记主模型连不上：只有 brain.ErrModelUnavailable 算失败，一次就打开，cooldown 后半开放一个请求去试
	primaryDown circuitbreaker.CircuitBreaker[any]
}

// NewFallbackChatModel 组合主备模型；cooldown 是主模型连不上之后多久内直接用备用模型（0 = 每次都先试主模型）。
func NewFallbackChatModel(primary brain.ChatModel, primaryName string, secondary brain.ChatModel, secondaryName string,
	cooldown time.Duration, logger *slog.Logger) *FallbackChatModel {
	return &FallbackChatModel{primary: primary, secondary: secondary, primaryName: primaryName, secondaryName: secondaryName,
		cooldown: cooldown, logger: logger,
		primaryDown: circuitbreaker.NewBuilder[any]().
			HandleErrors(brain.ErrModelUnavailable).
			WithFailureThreshold(1).
			WithSuccessThreshold(1).
			WithDelay(cooldown).
			Build()}
}

// StreamReply 见类型说明。
func (f *FallbackChatModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		if !f.primaryDown.TryAcquirePermit() { // 冷却中：不等主模型
			f.logger.Info("主模型冷却中，直接用备用模型", "secondary", f.secondaryName,
				"retry_in", f.primaryDown.RemainingDelay().Round(time.Second))
			f.streamSecondary(ctx, request, yield)
			return
		}
		// 拿到的名额每条路都要按结果还回去（半开时只放一个请求去试，不还就一直半开、再也不试主模型）：
		// 正常结束、调用方不要了都算主模型通着
		recorded := false
		record := func(err error) {
			if !recorded {
				recorded = true
				f.primaryDown.RecordError(err)
			}
		}
		defer record(nil)
		produced := false
		for delta, err := range f.primary.StreamReply(ctx, request) {
			if err != nil {
				record(err)
				if produced || errors.Is(err, context.Canceled) || ctx.Err() != nil {
					yield(brain.ReplyDelta{}, err)
					return
				}
				unavailable := errors.Is(err, brain.ErrModelUnavailable)
				f.logger.Warn("主模型失败，这一次改用备用模型", "primary", f.primaryName, "secondary", f.secondaryName,
					"unavailable", unavailable, "cooldown", f.cooldown, "err", err)
				f.streamSecondary(ctx, request, yield)
				return
			}
			if delta.Text != "" || len(delta.ToolCalls) > 0 {
				produced = true
			}
			if !yield(delta, nil) {
				return
			}
		}
	}
}

// streamSecondary 这一次由备用模型回答，原样转发。
func (f *FallbackChatModel) streamSecondary(ctx context.Context, request brain.ChatRequest, yield func(brain.ReplyDelta, error) bool) {
	for delta, err := range f.secondary.StreamReply(ctx, request) {
		if !yield(delta, err) || err != nil {
			return
		}
	}
}

// Warmup 预热主备两个模型里需要预热的（本地要把模型装进显存）。主模型预热失败不影响备用。
func (f *FallbackChatModel) Warmup(ctx context.Context) error {
	return errors.Join(brain.WarmUp(ctx, f.primary), brain.WarmUp(ctx, f.secondary))
}
