package spending

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/sqliteschema"
)

// recordTimeout 是每次请求收尾时记一笔最多等多久：账本是 SQLite，等写锁最多 sqliteschema.BusyTimeout，再等下去就是账本
// 出了问题；写不进的由账本留在内存里补写（见 pendingChargesLedger），回答的收尾不跟着一直等。
const recordTimeout = sqliteschema.BusyTimeout

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
// 模型必须在价格表里（要花钱的有单价、本地的标了不花钱）：不在 = 组装时漏了，按 0 日元记的话额度就管不住它，
// 所以直接让服务端起不来（价格配置缺了的情况，配置校验已经在这之前说明并退出）。
func NewMeteredChatModel(inner brain.ChatModel, model string, purpose Purpose, pricing Pricing, ledger Ledger,
	logger *slog.Logger) *MeteredChatModel {
	if !pricing.Knows(model) {
		panic(fmt.Sprintf("价格表里没有模型 %q（用途 %s）：组装时要把它加进价格表（要花钱的写单价，本地模型标 Free）", model, purpose))
	}
	return &MeteredChatModel{inner: inner, model: model, purpose: purpose, pricing: pricing, ledger: ledger, logger: logger}
}

// StreamReply 原样转发，每次请求都记一笔：正常结束时记最后一段带的用量；被打断、出错时记知道的那部分（标不完整），
// 一点用量都拿不到时也记一行（token 不知道），不漏记。
//
// 调用方中途不要了（回答被打断）而 ctx 已经取消时，接着把内层收完：内层很快因为取消结束，并交出知道的用量。
func (m *MeteredChatModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		recorded, callerGone := false, false
		defer func() {
			if !recorded {
				m.record(ctx, &brain.ReplyUsage{Incomplete: true, TokensUnknown: true})
			}
		}()
		for delta, err := range m.inner.StreamReply(ctx, request) {
			if delta.Usage != nil && !recorded {
				m.record(ctx, delta.Usage)
				recorded = true
			}
			if callerGone {
				continue
			}
			if !yield(delta, err) {
				if ctx.Err() == nil { // 没取消却不要了：内层还会接着生成，不等它，这次的用量就不知道了
					return
				}
				callerGone = true
				continue
			}
			if err != nil {
				return
			}
		}
	}
}

func (m *MeteredChatModel) record(ctx context.Context, usage *brain.ReplyUsage) {
	charge := Charge{Purpose: m.purpose, Item: "llm:" + m.model, InputTokens: usage.PromptTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens, Cost: m.pricing.LLMCost(m.model, *usage),
		Incomplete: usage.Incomplete, TokensUnknown: usage.TokensUnknown}
	// 不跟着这一轮取消：回答被打断（ctx 取消）时，已经产生的费用照样要记；期限另算（recordTimeout）。
	// 记上的每一笔由账本那层打日志；写不进的由账本留着补写，补写成功之前不放行有上限的付费调用，见 WithBookkeeping
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := m.ledger.Record(ctx, charge); err != nil {
		m.logger.Warn("💴 这次的用量没写进账本", "err", err, "item", charge.Item, "cost", charge.Cost.String(), "incomplete", charge.Incomplete)
	}
}

// Warmup 转发给里面的模型（它需要预热的话）。
func (m *MeteredChatModel) Warmup(ctx context.Context) error { return brain.WarmUp(ctx, m.inner) }

// BudgetedChatModel 按月度额度放行（R5）：额度内照常调用；超过上限就不调用，交出能念给主人的错误（额度用完了）。
// 不再改用本地模型顶替（第 7 段）。
type BudgetedChatModel struct {
	paid   brain.ChatModel
	guard  *Guard
	logger *slog.Logger
}

// NewBudgetedChatModel 创建按额度放行的模型。
func NewBudgetedChatModel(paid brain.ChatModel, guard *Guard, logger *slog.Logger) *BudgetedChatModel {
	return &BudgetedChatModel{paid: paid, guard: guard, logger: logger}
}

// StreamReply 每次请求前查一次额度。
func (b *BudgetedChatModel) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	err := b.guard.AllowPaidLLM(ctx)
	switch {
	case err == nil:
		return b.paid.StreamReply(ctx, request)
	case errors.Is(err, ErrOverBudget):
		b.logger.Warn("💴 月度额度已用完：不回答", "why", err)
		return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{}, err) }
	default: // 账本用不了（ErrLedgerUnavailable：读不出、写不进的补写不进去）：拿不到依据就不放行，告诉主人原因（否则账本一坏，花费就没有上限）
		b.logger.Error("💴 用量账本用不了：这一轮不调用要花钱的模型", "err", err)
		return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{}, err) }
	}
}

// Warmup 预热主用的那套（需要预热的话）。
func (b *BudgetedChatModel) Warmup(ctx context.Context) error { return brain.WarmUp(ctx, b.paid) }

// DailyCallLimit 限一个模型每天最多调用几次（按本地时区的日子算，只记在内存里）：给不受额度管的那次小调用兜底
// （月度上限到了以后，判断主人要不要提高上限），免得它成了没有上限的花费。
type DailyCallLimit struct {
	inner brain.ChatModel
	limit int
	now   func() time.Time

	mu   sync.Mutex
	day  string
	used int
}

// NewDailyCallLimit 创建每天最多调用 perDay 次的模型。
func NewDailyCallLimit(inner brain.ChatModel, perDay int) *DailyCallLimit {
	return &DailyCallLimit{inner: inner, limit: perDay, now: time.Now}
}

// StreamReply 今天的次数没用完就转发；用完了不调用，交出 ErrOverBudget。
func (d *DailyCallLimit) StreamReply(ctx context.Context, request brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	d.mu.Lock()
	if today := d.now().Format("2006-01-02"); today != d.day {
		d.day, d.used = today, 0
	}
	allowed := d.used < d.limit
	if allowed {
		d.used++
	}
	d.mu.Unlock()
	if !allowed {
		err := fmt.Errorf("%w：这个模型今天已经调用了 %d 次（每天的上限）", ErrOverBudget, d.limit)
		return func(yield func(brain.ReplyDelta, error) bool) { yield(brain.ReplyDelta{}, err) }
	}
	return d.inner.StreamReply(ctx, request)
}
