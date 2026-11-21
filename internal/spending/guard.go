package spending

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// Pricing 是把用量折成日元的单价（来自配置：云端价格以美元计，汇率也由配置给，都不写死在代码里）。
type Pricing struct {
	USDToYen float64 // 1 美元折多少日元
	// 每个用到的模型都要在表里：要花钱的写单价，本地模型标 Free。不在表里 = 组装漏了（记账模型建不起来，见 Knows）
	Models map[string]TokenPrice
	// 搜索服务每个积分的美元单价（Tavily 免费额度内为 0；R5 默认把搜索封在免费额度内）
	SearchCreditUSD float64
}

// TokenPrice 是每百万 token 的美元单价。
type TokenPrice struct {
	Free                bool // 不花钱（本地模型）：明确写出来，和「不在表里」分开
	InputUSDPerMillion  float64
	OutputUSDPerMillion float64
	// 提示词缓存：读缓存、写缓存的单价；0 = 按普通输入价算（宁可记多，不让额度失守）
	CacheReadUSDPerMillion  float64
	CacheWriteUSDPerMillion float64
}

// Knows 判断这个模型在不在价格表里（要花钱的写了单价，或者标了不花钱）。
func (p Pricing) Knows(model string) bool {
	_, ok := p.Models[model]
	return ok
}

// LLMYen 算一次模型调用折多少日元：没命中缓存的输入、缓存读、缓存写、输出各按各的价。
func (p Pricing) LLMYen(model string, usage brain.ReplyUsage) float64 {
	price, ok := p.Models[model]
	if !ok || price.Free { // 不在表里的建不出记账模型（NewMeteredChatModel），走不到这里
		return 0
	}
	cacheRead, cacheWrite := price.CacheReadUSDPerMillion, price.CacheWriteUSDPerMillion
	if cacheRead <= 0 {
		cacheRead = price.InputUSDPerMillion
	}
	if cacheWrite <= 0 {
		cacheWrite = price.InputUSDPerMillion
	}
	uncached := max(usage.PromptTokens-usage.CacheReadTokens-usage.CacheWriteTokens, 0)
	usd := (float64(uncached)*price.InputUSDPerMillion + float64(usage.CacheReadTokens)*cacheRead +
		float64(usage.CacheWriteTokens)*cacheWrite + float64(usage.OutputTokens)*price.OutputUSDPerMillion) / 1e6
	return usd * p.USDToYen
}

// SearchYen 算搜索积分折多少日元。
func (p Pricing) SearchYen(credits int) float64 {
	return float64(credits) * p.SearchCreditUSD * p.USDToYen
}

// Limits 是费用上限；0 = 不设上限。
type Limits struct {
	ResearchDailyYen   float64 // 后台调查每天最多花多少（默认 100 日元）
	MonthlyYen         float64 // 每月总费用上限：大模型 + 搜索，不分用途合计
	SearchDailyCredits int     // 搜索积分每天最多用多少（默认封在 Tavily 免费额度内：每月 1,000 ≈ 每天 30）
	WarnRatio          float64 // 月度用到这个比例时提醒一次（R5：0.8）
}

// ErrOverBudget 表示额度用完了（errors.Is 判断）。
var ErrOverBudget = errors.New("额度用完了")

// budgetError 是额度用完的具体说明：Is(ErrOverBudget)；SpokenMessage 让服务端直接念给主人，而不是报「出错了」。
type budgetError struct{ message string }

func (e *budgetError) Error() string         { return e.message }
func (e *budgetError) Is(target error) bool  { return target == ErrOverBudget }
func (e *budgetError) SpokenMessage() string { return e.message }

func overBudget(format string, args ...any) error {
	return &budgetError{message: fmt.Sprintf(format, args...)}
}

// ErrLedgerUnavailable 表示读不出用量账本（errors.Is 判断）：不知道花了多少，就不放行要花钱的调用。
var ErrLedgerUnavailable = errors.New("读不出用量账本")

// ledgerUnavailable 是读不出账本的具体错误：Is(ErrLedgerUnavailable)；SpokenMessage 念给主人听，让他知道为什么不回答。
type ledgerUnavailable struct{ err error }

func (e *ledgerUnavailable) Error() string        { return "读不出用量账本: " + e.err.Error() }
func (e *ledgerUnavailable) Unwrap() error        { return e.err }
func (e *ledgerUnavailable) Is(target error) bool { return target == ErrLedgerUnavailable }
func (e *ledgerUnavailable) SpokenMessage() string {
	return "我现在读不出费用账本，不知道这个月花了多少，为了不超出上限，先不回答了。"
}

// Guard 按账本决定还能不能花。时间按本地时区算「今天」「这个月」。
type Guard struct {
	ledger Ledger
	limits Limits
	logger *slog.Logger
	now    func() time.Time

	mu         sync.Mutex
	warnedFor  string // 已经提醒过的月份（"2026-11"），每月只提醒一次
	pendingMsg string // 还没告诉主人的提醒
}

// NewGuard 创建额度守卫。
func NewGuard(ledger Ledger, limits Limits, logger *slog.Logger) *Guard {
	return &Guard{ledger: ledger, limits: limits, logger: logger, now: time.Now}
}

func (g *Guard) startOfDay() time.Time {
	now := g.now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

func (g *Guard) startOfMonth() time.Time {
	now := g.now()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
}

// AllowResearch 判断今天还能不能派后台调查（以及调查进行中每一轮之前再判一次）。
func (g *Guard) AllowResearch(ctx context.Context) error {
	if err := g.allowMonthly(ctx); err != nil {
		return err
	}
	if g.limits.ResearchDailyYen <= 0 {
		return nil
	}
	spent, err := g.ledger.Total(ctx, g.startOfDay(), PurposeResearch)
	if err != nil {
		return err
	}
	if spent.Yen >= g.limits.ResearchDailyYen {
		return overBudget("今天的调查已经花了约 %.0f 日元（上限 %.0f 日元），明天再查吧。", spent.Yen, g.limits.ResearchDailyYen)
	}
	return nil
}

// AllowSearch 判断今天还能不能再用 credits 个搜索积分。
func (g *Guard) AllowSearch(ctx context.Context, credits int) error {
	if g.limits.SearchDailyCredits <= 0 {
		return nil
	}
	used, err := g.ledger.Total(ctx, g.startOfDay(), PurposeSearch, PurposeResearch)
	if err != nil {
		return err
	}
	if used.Credits+credits > g.limits.SearchDailyCredits {
		return overBudget("今天的搜索已经用了 %d 次（上限 %d 次），明天再搜吧。", used.Credits, g.limits.SearchDailyCredits)
	}
	return nil
}

// AllowPaidLLM 调用要花钱的大模型之前，看这个月的总费用（大模型 + 搜索）到没到上限；顺便在用到 WarnRatio 时准备一条提醒（每月一次）。
// 账本读不出时返回 ErrLedgerUnavailable：拿不到依据就不放行（否则账本一坏，花费就没有上限）。
func (g *Guard) AllowPaidLLM(ctx context.Context) error {
	return g.allowMonthly(ctx)
}

func (g *Guard) allowMonthly(ctx context.Context) error {
	if g.limits.MonthlyYen <= 0 {
		return nil
	}
	spent, err := g.ledger.Total(ctx, g.startOfMonth())
	if err != nil {
		return &ledgerUnavailable{err: err}
	}
	if spent.Yen >= g.limits.MonthlyYen {
		return overBudget("这个月的费用（大模型和搜索）已经约 %.0f 日元，到上限 %.0f 日元了，这个月先不回答了。", spent.Yen, g.limits.MonthlyYen)
	}
	if g.limits.WarnRatio > 0 && spent.Yen >= g.limits.MonthlyYen*g.limits.WarnRatio {
		month := g.startOfMonth().Format("2006-01")
		g.mu.Lock()
		if g.warnedFor != month {
			g.warnedFor = month
			g.pendingMsg = fmt.Sprintf("这个月的费用（大模型和搜索）已经用到约 %.0f 日元（上限 %.0f 日元，%.0f%%）。超过上限后改用本地模型或暂停回答。",
				spent.Yen, g.limits.MonthlyYen, spent.Yen/g.limits.MonthlyYen*100)
			g.logger.Warn("💴 "+g.pendingMsg, "spent_yen", spent.Yen, "limit_yen", g.limits.MonthlyYen)
		}
		g.mu.Unlock()
	}
	return nil
}

// Brief 实现 brain.Briefer：有还没告诉主人的额度提醒时附上；这一轮说完才算告诉了（被打断、出错时下一轮再附上）。
func (g *Guard) Brief(string) brain.Briefing {
	g.mu.Lock()
	msg := g.pendingMsg
	g.mu.Unlock()
	if msg == "" {
		return brain.Briefing{}
	}
	return brain.Briefing{Lines: []string{"额度提醒（找机会用一句话告诉主人，不用展开）：" + msg},
		Delivered: func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.pendingMsg == msg { // 这期间又换了新的提醒就留着新的
				g.pendingMsg = ""
			}
		}}
}

// SearchMeter 按用途给搜索查额度、记账（实现 tools.SearchMeter）：快查用 PurposeSearch，后台调查用 PurposeResearch。
type SearchMeter struct {
	guard   *Guard
	ledger  Ledger
	pricing Pricing
	purpose Purpose
	item    string // 例如 "tavily"
}

// NewSearchMeter 创建搜索计量。
func NewSearchMeter(guard *Guard, ledger Ledger, pricing Pricing, purpose Purpose, item string) *SearchMeter {
	return &SearchMeter{guard: guard, ledger: ledger, pricing: pricing, purpose: purpose, item: item}
}

// AllowSearch 查搜索积分的每日额度；后台调查还要查调查的每日额度。
func (m *SearchMeter) AllowSearch(ctx context.Context, credits int) error {
	if m.purpose == PurposeResearch {
		if err := m.guard.AllowResearch(ctx); err != nil {
			return err
		}
	}
	return m.guard.AllowSearch(ctx, credits)
}

// RecordSearch 记一笔搜索。
func (m *SearchMeter) RecordSearch(ctx context.Context, credits int) error {
	return m.ledger.Record(ctx, Charge{Purpose: m.purpose, Item: m.item, Credits: credits, Yen: m.pricing.SearchYen(credits)})
}
