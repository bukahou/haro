package spending

import (
	"context"
	"errors"
	"iter"
	"log/slog"

	"github.com/bukahou/haro/internal/brain"
)

// MeteredChatModel 给一个模型记账（装饰器）：每次请求结束时按 Usage 记一笔。
// 套在具体模型（云端 / 本地）外面，所以退路切换后记的是实际回答的那个模型。
type MeteredChatModel struct {
	inner   brain.ChatModel
	model   string // 计价用的模型名（Pricing.Models 的键）
	purpose Purpose
	pricing Pricing
	ledger  Ledger
	logger  *slog.Logger
}

// NewMeteredChatModel 创建记账的模型。
func NewMeteredChatModel(inner brain.ChatModel, model string, purpose Purpose, pricing Pricing, ledger Ledger,
	logger *slog.Logger) *MeteredChatModel {
	return &MeteredChatModel{inner: inner, model: model, purpose: purpose, pricing: pricing, ledger: ledger, logger: logger}
}

// StreamReply 原样转发，最后一段带 Usage 时记账。
func (m *MeteredChatModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for delta, err := range m.inner.StreamReply(ctx, messages, tools) {
			if delta.Usage != nil {
				m.record(delta.Usage)
			}
			if !yield(delta, err) || err != nil {
				return
			}
		}
	}
}

func (m *MeteredChatModel) record(usage *brain.ReplyUsage) {
	charge := Charge{Purpose: m.purpose, Item: "llm:" + m.model, InputTokens: usage.PromptTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens, Yen: m.pricing.LLMYen(m.model, *usage)}
	// 用独立的 context：回答被打断（ctx 取消）时，已经产生的费用照样要记
	if err := m.ledger.Record(context.Background(), charge); err != nil {
		m.logger.Warn("用量没记上", "err", err, "yen", charge.Yen)
	}
}

// Warmup 原样转发。
func (m *MeteredChatModel) Warmup(ctx context.Context) error { return m.inner.Warmup(ctx) }

// BudgetedChatModel 按月度额度选模型（R5 c）：额度内用 paid（可能花钱的那套组合）；
// 超过上限改用 free（本地模型）；没有 free 时回答「额度用完了」。
type BudgetedChatModel struct {
	paid, free brain.ChatModel
	guard      *Guard
	logger     *slog.Logger
}

// NewBudgetedChatModel 创建按额度选模型的组合；free 可以为 nil。
func NewBudgetedChatModel(paid, free brain.ChatModel, guard *Guard, logger *slog.Logger) *BudgetedChatModel {
	return &BudgetedChatModel{paid: paid, free: free, guard: guard, logger: logger}
}

// StreamReply 每次请求前查一次额度。
func (b *BudgetedChatModel) StreamReply(ctx context.Context, messages []brain.ChatMessage, tools []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	err := b.guard.AllowPaidLLM(ctx)
	switch {
	case err == nil:
		return b.paid.StreamReply(ctx, messages, tools)
	case errors.Is(err, ErrOverBudget) && b.free != nil:
		b.logger.Warn("💴 月度额度已用完：这一轮用本地模型", "why", err)
		return b.free.StreamReply(ctx, messages, tools)
	case errors.Is(err, ErrOverBudget):
		b.logger.Warn("💴 月度额度已用完，又没有本地模型可退：不回答", "why", err)
		return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{}, err) }
	default: // 账本读不出来：不因为记账故障让人用不了，照常回答
		b.logger.Warn("读不出用量账本，这一轮不查额度", "err", err)
		return b.paid.StreamReply(ctx, messages, tools)
	}
}

// Warmup 预热主用的那套。
func (b *BudgetedChatModel) Warmup(ctx context.Context) error { return b.paid.Warmup(ctx) }
