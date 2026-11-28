package spending

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/money"
)

// Pricing 是把用量算成钱的单价：服务商按美元收费，所以算出来的是原始的美元金额（换成默认货币在汇总时做，见 money.Converter）。
type Pricing struct {
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
	// Tiers：一次请求的输入（含缓存读写）超过 AboveInputTokens 时，整次按那一档的单价算（例如上下文超过 10 万 token）。
	// 按门槛从低到高
	Tiers []PriceTier
	// LongCacheWriteFactor：写进留得久的缓存的输入（ReplyUsage.CacheWriteLongTokens）按那一档输入价的几倍算
	// （目录里没有这一项，internal/pricecatalog 按厂商填）；0 = 和一般的缓存写同价
	LongCacheWriteFactor float64
}

// PriceTier 是按输入量分的一档单价。
type PriceTier struct {
	AboveInputTokens int
	Price            TokenPrice // 用其中的四个单价
}

// forInput 是输入 promptTokens 个 token 的那次请求用的单价：超过了哪一档的门槛就用那一档（取最高的），都没超过用平常的。
func (p TokenPrice) forInput(promptTokens int) TokenPrice {
	chosen := p
	for _, tier := range p.Tiers {
		if promptTokens > tier.AboveInputTokens {
			chosen = tier.Price
		}
	}
	return chosen
}

// Knows 判断这个模型在不在价格表里（要花钱的写了单价，或者标了不花钱）。
func (p Pricing) Knows(model string) bool {
	_, ok := p.Models[model]
	return ok
}

// LLMCost 算一次模型调用花了多少（美元）：没命中缓存的输入、缓存读、缓存写、输出各按各的价。
func (p Pricing) LLMCost(model string, usage brain.ReplyUsage) money.Amount {
	price, ok := p.Models[model]
	if !ok || price.Free { // 不在表里的建不出记账模型（NewMeteredChatModel），走不到这里
		return money.Amount{Currency: money.USD}
	}
	longFactor := price.LongCacheWriteFactor
	price = price.forInput(usage.PromptTokens)
	cacheRead, cacheWrite := price.CacheReadUSDPerMillion, price.CacheWriteUSDPerMillion
	if cacheRead <= 0 {
		cacheRead = price.InputUSDPerMillion
	}
	if cacheWrite <= 0 {
		cacheWrite = price.InputUSDPerMillion
	}
	longWrite := min(usage.CacheWriteLongTokens, usage.CacheWriteTokens)
	longWritePrice := cacheWrite
	if longFactor > 0 {
		longWritePrice = price.InputUSDPerMillion * longFactor
	}
	uncached := max(usage.PromptTokens-usage.CacheReadTokens-usage.CacheWriteTokens, 0)
	usd := (float64(uncached)*price.InputUSDPerMillion + float64(usage.CacheReadTokens)*cacheRead +
		float64(usage.CacheWriteTokens-longWrite)*cacheWrite + float64(longWrite)*longWritePrice +
		float64(usage.OutputTokens)*price.OutputUSDPerMillion) / 1e6
	return money.Of(usd, money.USD)
}

// SearchCost 算搜索积分花了多少（美元）。
func (p Pricing) SearchCost(credits int) money.Amount {
	return money.Of(float64(credits)*p.SearchCreditUSD, money.USD)
}

// DefaultWarnRatio 是月度费用用到上限的这个比例时提醒一次（提醒的话由 Guard 交给前台）。
const DefaultWarnRatio = 0.8

// Limits 是费用上限：金额都是默认货币（Currency）的；0 = 不设这一项。账本按原始币种记，判断时换成默认货币再比。
type Limits struct {
	Currency           string       // 默认货币（ISO 4217）：上限、念给主人的金额都用它
	ResearchDaily      money.Amount // 后台调查每天最多花多少
	Monthly            money.Amount // 每月总费用上限：大模型 + 搜索，不分用途合计
	SearchDailyCredits int          // 搜索积分每天最多用多少（默认封在 Tavily 免费额度内：每月 1,000 ≈ 每天 30）
	WarnRatio          float64      // 月度用到这个比例时提醒一次（DefaultWarnRatio）；0 = 不提醒
	// MonthlyRaise：主人口头确认一次，这个月的上限临时提高多少；0 = 不能口头提高（到上限就停到下个月）
	MonthlyRaise money.Amount
}

// ErrOverBudget 表示额度用完了（errors.Is 判断）。
var ErrOverBudget = errors.New("额度用完了")

// budgetError 是额度用完的具体说明：Is(ErrOverBudget)；月度总上限的那种还 Is(brain.ErrMonthlyCapReached)。
// Phrase 让服务端按主人的语言念出原因，而不是报「出错了」。
type budgetError struct {
	message string            // 日志、交给模型的工具结果里用
	phrase  string            // 念给主人的那句（brain.Phrase*）
	values  map[string]string // 填进那句话里的数
	monthly bool              // 是每月总费用到了上限
}

func (e *budgetError) Error() string { return e.message }
func (e *budgetError) Is(target error) bool {
	return target == ErrOverBudget || (e.monthly && target == brain.ErrMonthlyCapReached)
}
func (e *budgetError) Phrase() (string, map[string]string) { return e.phrase, e.values }

// overBudget 用花了多少、上限多少造一个额度用完的错误：spentName 是那句话里「花了多少」的名字；
// 金额的值另带币种（brain.PhraseCurrency），那句话按主人的语言念币种。
func overBudget(phrase, spentName string, spent, limit string, currency string, format string, args ...any) error {
	values := map[string]string{spentName: spent, "limit": limit}
	if currency != "" {
		values[brain.PhraseCurrency] = currency
	}
	return &budgetError{message: fmt.Sprintf(format, args...), phrase: phrase, values: values}
}

// ErrLedgerUnavailable 表示用量账本用不了（errors.Is 判断）：读不出，或者之前写不进的补写不进去。
// 不知道一共花了多少，就不放行要花钱的调用。
var ErrLedgerUnavailable = errors.New("用量账本用不了")

// ledgerUnavailable 是账本用不了的具体错误：Is(ErrLedgerUnavailable)；Phrase 念给主人听，让他知道为什么不回答。
type ledgerUnavailable struct{ err error }

func (e *ledgerUnavailable) Error() string        { return "用量账本用不了: " + e.err.Error() }
func (e *ledgerUnavailable) Unwrap() error        { return e.err }
func (e *ledgerUnavailable) Is(target error) bool { return target == ErrLedgerUnavailable }
func (e *ledgerUnavailable) Phrase() (string, map[string]string) {
	return brain.PhraseLedgerUnavailable, nil
}

// Guard 按账本决定还能不能花。时间按本地时区算「今天」「这个月」。
type Guard struct {
	ledger Ledger
	limits Limits
	rates  money.Converter // 账本里不是默认货币的（服务商的美元）换成默认货币
	logger *slog.Logger
	now    func() time.Time

	// accountOf、caps：分账号（UseAccounts）。没接时一切都算管理员的、只有全局上限
	accountOf AccountOf
	caps      AccountCaps

	mu         sync.Mutex
	warnedFor  string // 已经提醒过的月份（"2026-11"），每月只提醒一次
	pendingMsg string // 还没告诉主人的提醒
}

// AccountCaps 是每个账号的月上限（accounts.Store 实现）：零值 = 不设。管理员（账号为空）用全局的上限，不问它。
type AccountCaps interface {
	MonthlyCapOf(ctx context.Context, account string) (money.Amount, error)
}

// UseAccounts 接上账号：ctx 里标的人（brain.WithBillingPerson）按 accountOf 换成账号，按账号的月上限判断；
// 全局的额度提醒、口头提高上限只给管理员。启动时、放行任何调用之前调用一次。
func (g *Guard) UseAccounts(accountOf AccountOf, caps AccountCaps) {
	g.accountOf, g.caps = accountOf, caps
}

// accountOfPerson 是这个人所在的账号（空 = 管理员；没接账号时都是管理员）。
func (g *Guard) accountOfPerson(person string) string {
	if g.accountOf == nil {
		return ""
	}
	return g.accountOf(person)
}

// account 是 ctx 里标的人所在的账号。
func (g *Guard) account(ctx context.Context) string {
	return g.accountOfPerson(brain.BillingPersonFrom(ctx))
}

// NewGuard 创建额度守卫。rates 把账本的原始金额换成默认货币（limits.Currency）。
func NewGuard(ledger Ledger, limits Limits, rates money.Converter, logger *slog.Logger) *Guard {
	return &Guard{ledger: ledger, limits: limits, rates: rates, logger: logger, now: time.Now}
}

func (g *Guard) startOfDay() time.Time {
	now := g.now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

func (g *Guard) startOfMonth() time.Time {
	now := g.now()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
}

// spentSince 合计 since 之后的花费（purposes 为空 = 所有用途），拿来判断额度：先把之前写不进账本的补写进去。
// 补写不成、读不出时返回 ErrLedgerUnavailable：合计不完整（写不进时它不再增长），不能拿来放行。
func (g *Guard) spentSince(ctx context.Context, since time.Time, purposes ...Purpose) (Totals, error) {
	return g.spentOf(ctx, nil, since, purposes...)
}

// spentOf 同 spentSince，account 不为 nil 时只算这个账号的。
func (g *Guard) spentOf(ctx context.Context, account *string, since time.Time, purposes ...Purpose) (Totals, error) {
	if err := g.ledger.FlushPending(ctx); err != nil {
		return Totals{}, &ledgerUnavailable{err: err}
	}
	var spent Totals
	var err error
	if account == nil {
		spent, err = g.ledger.Total(ctx, since, purposes...)
	} else {
		spent, err = g.ledger.AccountTotal(ctx, *account, since, purposes...)
	}
	if err != nil {
		return Totals{}, &ledgerUnavailable{err: err}
	}
	return spent, nil
}

// inDefault 把按币种分开的合计换成默认货币。换算不了（没有汇率）时按账本用不了处理：不知道花了多少就不放行。
func (g *Guard) inDefault(sum money.Sum) (money.Amount, error) {
	total, err := g.rates.Convert(sum, g.limits.Currency)
	if err != nil {
		return money.Amount{}, &ledgerUnavailable{err: err}
	}
	return total.Amount, nil
}

// AllowResearch 判断今天还能不能派后台调查（以及调查进行中每一轮之前再判一次）。
func (g *Guard) AllowResearch(ctx context.Context) error {
	if err := g.allowMonthly(ctx); err != nil {
		return err
	}
	if g.limits.ResearchDaily.Micros <= 0 {
		return nil
	}
	totals, err := g.spentSince(ctx, g.startOfDay(), PurposeResearch)
	if err != nil {
		return err
	}
	spent, err := g.inDefault(totals.Spent)
	if err != nil {
		return err
	}
	if spent.Micros >= g.limits.ResearchDaily.Micros {
		return overBudget(brain.PhraseOverResearchDaily, "spent", spent.Rounded(), g.limits.ResearchDaily.Rounded(), g.limits.Currency,
			"今天的调查已经花了约 %s（上限 %s），明天再查吧。", spent, g.limits.ResearchDaily)
	}
	return nil
}

// AllowSearch 判断今天还能不能再用 credits 个搜索积分。
func (g *Guard) AllowSearch(ctx context.Context, credits int) error {
	if g.limits.SearchDailyCredits <= 0 {
		return nil
	}
	used, err := g.spentSince(ctx, g.startOfDay(), PurposeSearch, PurposeResearch)
	if err != nil {
		return err
	}
	if used.Credits+credits > g.limits.SearchDailyCredits {
		return overBudget(brain.PhraseOverSearchDaily, "used", strconv.Itoa(used.Credits), strconv.Itoa(g.limits.SearchDailyCredits), "",
			"今天的搜索已经用了 %d 次（上限 %d 次），明天再搜吧。", used.Credits, g.limits.SearchDailyCredits)
	}
	return nil
}

// AllowPaidLLM 调用要花钱的大模型之前，看这个月的总费用（大模型 + 搜索）到没到上限；顺便在用到 WarnRatio 时准备一条提醒（每月一次）。
// 账本读不出、之前写不进的补写不进去时返回 ErrLedgerUnavailable：拿不到依据就不放行（否则账本一坏，花费就没有上限）。
func (g *Guard) AllowPaidLLM(ctx context.Context) error {
	return g.allowMonthly(ctx)
}

// allowMonthly 先看全局的月度上限，再看这个账号自己的。
func (g *Guard) allowMonthly(ctx context.Context) error {
	account := g.account(ctx)
	if err := g.allowGlobalMonthly(ctx, account); err != nil {
		return err
	}
	return g.allowAccountMonthly(ctx, account)
}

// allowAccountMonthly 看这个账号这个月的花费到没到管理员给它定的上限（管理员、没定上限的不看）。
func (g *Guard) allowAccountMonthly(ctx context.Context, account string) error {
	limit, err := g.accountCap(ctx, account)
	if err != nil || limit.Micros <= 0 {
		return err
	}
	totals, err := g.spentOf(ctx, &account, g.startOfMonth())
	if err != nil {
		return err
	}
	spent, err := g.inDefault(totals.Spent)
	if err != nil {
		return err
	}
	if spent.Micros >= limit.Micros {
		return &budgetError{message: fmt.Sprintf("这个账号这个月的费用已经约 %s，到了管理员定的上限 %s。", spent, limit),
			phrase: brain.PhraseOverAccountMonthly, values: map[string]string{"spent": spent.Rounded(), "limit": limit.Rounded(),
				brain.PhraseCurrency: g.limits.Currency}}
	}
	return nil
}

// accountCap 是这个账号的月上限，换成默认货币；管理员、没接账号、没定的为零值。
func (g *Guard) accountCap(ctx context.Context, account string) (money.Amount, error) {
	if account == "" || g.caps == nil {
		return money.Amount{}, nil
	}
	limit, err := g.caps.MonthlyCapOf(ctx, account)
	if err != nil {
		return money.Amount{}, &ledgerUnavailable{err: err}
	}
	if limit.Micros <= 0 {
		return money.Amount{}, nil
	}
	return g.inDefault(money.Sum{limit.Currency: limit.Micros})
}

// allowGlobalMonthly 看全局（所有账号一起）的月度上限；用到提醒的比例时准备一条提醒（只告诉管理员）。
func (g *Guard) allowGlobalMonthly(ctx context.Context, account string) error {
	if g.limits.Monthly.Micros <= 0 {
		return nil
	}
	totals, err := g.spentSince(ctx, g.startOfMonth())
	if err != nil {
		return err
	}
	spent, err := g.inDefault(totals.Spent)
	if err != nil {
		return err
	}
	limit, err := g.monthlyLimit(ctx)
	if err != nil {
		return err
	}
	if spent.Micros >= limit.Micros {
		return g.overMonthly(spent, limit, account == "")
	}
	if g.limits.WarnRatio > 0 && spent.Value() >= limit.Value()*g.limits.WarnRatio {
		month := g.startOfMonth().Format("2006-01")
		g.mu.Lock()
		if g.warnedFor != month {
			g.warnedFor = month
			g.pendingMsg = fmt.Sprintf("这个月的费用（大模型和搜索）已经用到约 %s（上限 %s，%.0f%%）。超过上限后暂停回答（主人可以口头临时提高上限）。",
				spent, limit, spent.Value()/limit.Value()*100)
			g.logger.Warn("💴 "+g.pendingMsg, "spent", spent.String(), "limit", limit.String())
		}
		g.mu.Unlock()
	}
	return nil
}

// monthlyLimit 是这个月的上限：配置的上限 + 这个月主人口头提高的合计（都换成默认货币）。
func (g *Guard) monthlyLimit(ctx context.Context) (money.Amount, error) {
	raised, err := g.ledger.CapRaised(ctx, g.startOfMonth())
	if err != nil {
		return money.Amount{}, &ledgerUnavailable{err: err}
	}
	raisedTotal, err := g.inDefault(raised)
	if err != nil {
		return money.Amount{}, err
	}
	return money.Amount{Micros: g.limits.Monthly.Micros + raisedTotal.Micros, Currency: g.limits.Currency}, nil
}

// overMonthly 是月度总费用到了上限的错误：管理员（admin）能口头提高时，念的那句顺带说怎么提高；
// 别的账号只说到了上限（不能提高，也不进「要不要提高」的判断）。
func (g *Guard) overMonthly(spent, limit money.Amount, admin bool) error {
	message := fmt.Sprintf("这个月的费用（大模型和搜索）已经约 %s，到上限 %s 了，这个月先不回答了。", spent, limit)
	values := map[string]string{"spent": spent.Rounded(), "limit": limit.Rounded(), brain.PhraseCurrency: g.limits.Currency}
	phrase := brain.PhraseOverMonthlyBudget
	if admin && g.limits.MonthlyRaise.Micros > 0 {
		phrase, values["raise"] = brain.PhraseOverMonthlyBudgetRaisable, g.limits.MonthlyRaise.Rounded()
	}
	return &budgetError{message: message, phrase: phrase, values: values, monthly: admin}
}

// RaiseMonthlyCap 实现 brain.MonthlyCap：主人口头确认后，把这个月的上限提高 MonthlyRaise（记进账本，重启后照样算），
// 返回提高后的上限。执行方自己守门：ctx 里没有 person 对提高上限的确认（brain.WithConfirmation）就不提高，交回 brain.ErrNotConfirmed。
func (g *Guard) RaiseMonthlyCap(ctx context.Context, person string) (money.Amount, error) {
	if _, err := brain.RequireConfirmation(ctx, brain.RaiseMonthlyCapTool, person); err != nil {
		return money.Amount{}, err
	}
	if g.accountOfPerson(person) != "" { // 全局的上限只有管理员能提高（执行方自己守门）
		return money.Amount{}, errors.New("只有管理员能提高这个月的上限")
	}
	if g.limits.Monthly.Micros <= 0 || g.limits.MonthlyRaise.Micros <= 0 {
		return money.Amount{}, errors.New("没有设月度上限，或者不允许口头提高")
	}
	if err := g.ledger.RaiseCap(ctx, CapRaise{At: g.now(), Amount: g.limits.MonthlyRaise, Person: person}); err != nil {
		return money.Amount{}, fmt.Errorf("记不下提高的上限: %w", err)
	}
	return g.monthlyLimit(ctx)
}

// Brief 实现 brain.Briefer：有还没告诉主人的额度提醒时附上（全局的额度只告诉管理员）；这一轮说完才算告诉了（被打断、出错时下一轮再附上）。
func (g *Guard) Brief(person string) brain.Briefing {
	if g.accountOfPerson(person) != "" {
		return brain.Briefing{}
	}
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

// SearchMeter 按用途给搜索查额度、记账（实现 tools.SearchMeter、research.Meter）：快查用 PurposeSearch，后台调查用 PurposeResearch。
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

// RecordSearch 记一笔发出去的搜索请求（实现 tools.SearchRecorder、research.Recorder）：没拿到结果的标不完整，积分照记（服务商可能已经扣了）。
// 不跟着调用方取消：请求被打断时，已经发出去的照样要记；期限另算（recordTimeout）。
func (m *SearchMeter) RecordSearch(ctx context.Context, credits int, incomplete bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	return m.ledger.Record(ctx, Charge{Purpose: m.purpose, Item: m.item, Credits: credits, Cost: m.pricing.SearchCost(credits),
		Incomplete: incomplete})
}
