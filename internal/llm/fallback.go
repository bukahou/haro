package llm

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// FallbackChatModel 先用主模型；主模型在**产出任何内容之前**就失败了（断网、超时、服务报错），改用备用模型。
// 已经开始说了才失败的不切换（否则会说两遍），直接报错。现在只用在「更强的模型出错时由原来那套云端模型接手」：
// 云端不再自动换本地模型（第 7 段）。
//
// 熔断：主模型连不上（brain.ErrModelUnavailable）后的 Cooldown 内直接用备用模型、不再试主模型；过了 Cooldown 再试一次，
// 成功就切回去。否则一句话里每轮推理（用工具时有两三轮）都要先等主模型超时（2026-11-15 实测断网时等了 17 s）。
// 别的失败（拒答、请求被拒）只针对这一次请求：这一次改用备用模型，下一次照常先试主模型。
type FallbackChatModel struct {
	Primary, Secondary         brain.ChatModel
	PrimaryName, SecondaryName string // 用于日志，例如 "strong" / "cloud"
	Cooldown                   time.Duration
	Logger                     *slog.Logger

	mu            sync.Mutex
	degradedUntil time.Time // 在这之前直接用备用模型
}

// NewFallbackChatModel 组合主备模型。
func NewFallbackChatModel(primary brain.ChatModel, primaryName string, secondary brain.ChatModel, secondaryName string,
	cooldown time.Duration, logger *slog.Logger) *FallbackChatModel {
	return &FallbackChatModel{Primary: primary, Secondary: secondary, PrimaryName: primaryName,
		SecondaryName: secondaryName, Cooldown: cooldown, Logger: logger}
}

func (f *FallbackChatModel) degraded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Now().Before(f.degradedUntil)
}

func (f *FallbackChatModel) markPrimaryFailed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.degradedUntil = time.Now().Add(f.Cooldown)
}

// StreamReply 见类型说明。
func (f *FallbackChatModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		if f.degraded() { // 熔断中：不等主模型
			f.Logger.Info("主模型熔断中，直接用备用模型", "secondary", f.SecondaryName)
			for delta, err := range f.Secondary.StreamReply(ctx, request) {
				if !yield(delta, err) || err != nil {
					return
				}
			}
			return
		}
		produced := false
		for delta, err := range f.Primary.StreamReply(ctx, request) {
			if err != nil {
				if produced || errors.Is(err, context.Canceled) || ctx.Err() != nil {
					yield(brain.ReplyDelta{}, err)
					return
				}
				unavailable := errors.Is(err, brain.ErrModelUnavailable)
				if unavailable { // 连不上会持续一阵：冷却期内不再先等主模型超时
					f.markPrimaryFailed()
				}
				f.Logger.Warn("主模型失败，这一次改用备用模型", "primary", f.PrimaryName, "secondary", f.SecondaryName,
					"unavailable", unavailable, "cooldown", f.Cooldown, "err", err)
				for delta, err := range f.Secondary.StreamReply(ctx, request) {
					if !yield(delta, err) || err != nil {
						return
					}
				}
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

// Warmup 预热主备两个模型里需要预热的（本地要把模型装进显存）。主模型预热失败不影响备用。
func (f *FallbackChatModel) Warmup(ctx context.Context) error {
	return errors.Join(brain.WarmUp(ctx, f.Primary), brain.WarmUp(ctx, f.Secondary))
}
