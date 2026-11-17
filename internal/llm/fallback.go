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

// FallbackChatModel 先用主模型（云端）；主模型在**产出任何内容之前**就失败了（断网、超时、服务报错），
// 改用备用模型（本地）。已经开始说了才失败的不切换（否则会说两遍），直接报错。
//
// 熔断：主模型失败后的 Cooldown 内直接用备用模型、不再试主模型；过了 Cooldown 再试一次，成功就切回去。
// 否则一句话里每轮推理（用工具时有两三轮）都要先等主模型超时（2026-11-15 实测断网时等了 17 s）。
type FallbackChatModel struct {
	Primary, Secondary         brain.ChatModel
	PrimaryName, SecondaryName string // 用于日志，例如 "cloud" / "local"
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
func (f *FallbackChatModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		if f.degraded() { // 熔断中：不等主模型
			f.Logger.Info("主模型熔断中，直接用备用模型", "secondary", f.SecondaryName)
			for delta, err := range f.Secondary.StreamReply(ctx, messages, tools) {
				if !yield(delta, err) || err != nil {
					return
				}
			}
			return
		}
		produced := false
		for delta, err := range f.Primary.StreamReply(ctx, messages, tools) {
			if err != nil {
				if produced || errors.Is(err, context.Canceled) || ctx.Err() != nil {
					yield(brain.ReplyDelta{}, err)
					return
				}
				f.markPrimaryFailed()
				f.Logger.Warn("主模型失败，改用备用模型", "primary", f.PrimaryName, "secondary", f.SecondaryName,
					"cooldown", f.Cooldown, "err", err)
				for delta, err := range f.Secondary.StreamReply(ctx, messages, tools) {
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

// Warmup 预热主备两个模型（本地要把模型装进显存；云端的预热是空操作）。主模型预热失败不影响备用。
func (f *FallbackChatModel) Warmup(ctx context.Context) error {
	return errors.Join(f.Primary.Warmup(ctx), f.Secondary.Warmup(ctx))
}
