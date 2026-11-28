package spending

import (
	"context"
	"time"

	"github.com/bukahou/haro/internal/money"
)

// SpendingStatus 是这个月的花费离上限多远。
type SpendingStatus string

// 状态（和 App 契约的 SpendingStatus 同一组值）。
const (
	StatusOK      SpendingStatus = "ok"      // 正常
	StatusNear    SpendingStatus = "near"    // 到了提醒的比例（WarnRatio）
	StatusReached SpendingStatus = "reached" // 到了上限，暂停回答
)

// Spent 是一笔合计：按币种分开的原始合计，和换成默认货币之后的。
type Spent struct {
	ByCurrency money.Sum
	Total      money.Total
}

// PurposeSpent 是一个用途这个月花了多少。
type PurposeSpent struct {
	Purpose Purpose
	Spent   Spent
}

// Summary 是花费的汇总（App 的花费页、概览用）：金额都换成默认货币，带换算说明。
type Summary struct {
	Currency      string
	Month         Spent
	MonthCap      money.Amount // 这个月的上限（含口头提高的）；零值 = 不设
	Status        SpendingStatus
	CanRaise      bool         // 到上限后能不能口头临时提高
	Raise         money.Amount // 一次提高多少（CanRaise 时）
	ByPurpose     []PurposeSpent
	TodayResearch Spent
	ResearchCap   money.Amount // 零值 = 不设
	SearchCredits int          // 今天用了几个搜索积分（快查 + 调查）
	SearchCap     int          // 0 = 不限
}

// summaryPurposes 是汇总里按用途分开列的用途（和记账时的用途同一组）。
var summaryPurposes = []Purpose{PurposeChat, PurposeStrong, PurposeVision, PurposeSummary, PurposeResearch, PurposeSearch}

// Summary 汇总这个月和今天的花费。账本读不出、换算不了时报错（不编数字）。
func (g *Guard) Summary(ctx context.Context) (Summary, error) {
	summary := Summary{Currency: g.limits.Currency, ResearchCap: g.limits.ResearchDaily, SearchCap: g.limits.SearchDailyCredits}
	month, err := g.spent(ctx, g.startOfMonth())
	if err != nil {
		return Summary{}, err
	}
	summary.Month = month
	summary.Status = StatusOK
	if g.limits.Monthly.Micros > 0 {
		limit, err := g.monthlyLimit(ctx)
		if err != nil {
			return Summary{}, err
		}
		summary.MonthCap = limit
		spent := month.Total.Amount
		switch {
		case spent.Micros >= limit.Micros:
			summary.Status = StatusReached
		case g.limits.WarnRatio > 0 && spent.Value() >= limit.Value()*g.limits.WarnRatio:
			summary.Status = StatusNear
		}
		summary.CanRaise, summary.Raise = g.limits.MonthlyRaise.Micros > 0, g.limits.MonthlyRaise
	}
	for _, purpose := range summaryPurposes {
		spent, err := g.spent(ctx, g.startOfMonth(), purpose)
		if err != nil {
			return Summary{}, err
		}
		summary.ByPurpose = append(summary.ByPurpose, PurposeSpent{Purpose: purpose, Spent: spent})
	}
	if summary.TodayResearch, err = g.spent(ctx, g.startOfDay(), PurposeResearch); err != nil {
		return Summary{}, err
	}
	used, err := g.spentSince(ctx, g.startOfDay(), PurposeSearch, PurposeResearch)
	if err != nil {
		return Summary{}, err
	}
	summary.SearchCredits = used.Credits
	return summary, nil
}

// spent 合计 since 之后的花费（purposes 为空 = 所有用途）并换成默认货币。
func (g *Guard) spent(ctx context.Context, since time.Time, purposes ...Purpose) (Spent, error) {
	return g.spentFor(ctx, nil, since, purposes...)
}

// spentFor 同 spent，account 不为 nil 时只算这个账号的。
func (g *Guard) spentFor(ctx context.Context, account *string, since time.Time, purposes ...Purpose) (Spent, error) {
	totals, err := g.spentOf(ctx, account, since, purposes...)
	if err != nil {
		return Spent{}, err
	}
	total, err := g.rates.Convert(totals.Spent, g.limits.Currency)
	if err != nil {
		return Spent{}, &ledgerUnavailable{err: err}
	}
	return Spent{ByCurrency: totals.Spent, Total: total}, nil
}

// AccountSpent 是一个账号（空 = 管理员自己）这个月花了多少（管理员的账号列表用）。
func (g *Guard) AccountSpent(ctx context.Context, account string) (Spent, error) {
	return g.spentFor(ctx, &account, g.startOfMonth())
}

// AccountCap 是一个账号的月上限（默认货币；管理员、没定的为零值）。
func (g *Guard) AccountCap(ctx context.Context, account string) (money.Amount, error) {
	return g.accountCap(ctx, account)
}

// SummaryFor 是一个账号看到的花费汇总：管理员（account 为空）看全局的（Summary，所有账号一起、全局的上限）；
// 别的账号只看自己的：这个月、按用途、今天的调查和搜索，上限是管理员给它定的（不能口头提高；全局每天的调查、搜索上限只给管理员看）。
func (g *Guard) SummaryFor(ctx context.Context, account string) (Summary, error) {
	if account == "" {
		return g.Summary(ctx)
	}
	summary := Summary{Currency: g.limits.Currency, Status: StatusOK}
	month, err := g.spentFor(ctx, &account, g.startOfMonth())
	if err != nil {
		return Summary{}, err
	}
	summary.Month = month
	limit, err := g.accountCap(ctx, account)
	if err != nil {
		return Summary{}, err
	}
	if limit.Micros > 0 {
		summary.MonthCap = limit
		spent := month.Total.Amount
		switch {
		case spent.Micros >= limit.Micros:
			summary.Status = StatusReached
		case g.limits.WarnRatio > 0 && spent.Value() >= limit.Value()*g.limits.WarnRatio:
			summary.Status = StatusNear
		}
	}
	for _, purpose := range summaryPurposes {
		spent, err := g.spentFor(ctx, &account, g.startOfMonth(), purpose)
		if err != nil {
			return Summary{}, err
		}
		summary.ByPurpose = append(summary.ByPurpose, PurposeSpent{Purpose: purpose, Spent: spent})
	}
	if summary.TodayResearch, err = g.spentFor(ctx, &account, g.startOfDay(), PurposeResearch); err != nil {
		return Summary{}, err
	}
	used, err := g.spentOf(ctx, &account, g.startOfDay(), PurposeSearch, PurposeResearch)
	if err != nil {
		return Summary{}, err
	}
	summary.SearchCredits = used.Credits
	return summary, nil
}
