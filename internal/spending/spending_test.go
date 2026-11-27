package spending

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"iter"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func openTestLedger(t *testing.T) Ledger {
	t.Helper()
	ledger, err := OpenLedger(context.Background(), filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	return ledger
}

func TestLedgerTotalsByTimeAndPurpose(t *testing.T) {
	ctx := context.Background()
	ledger := openTestLedger(t)
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)
	for _, charge := range []Charge{
		{At: now.AddDate(0, 0, -1), Purpose: PurposeResearch, Item: "tavily", Credits: 3, Yen: 50}, // 昨天
		{At: now, Purpose: PurposeResearch, Item: "llm:x", Yen: 20},
		{At: now, Purpose: PurposeSearch, Item: "tavily", Credits: 2},
		{At: now, Purpose: PurposeChat, Item: "llm:x", Yen: 5},
	} {
		if err := ledger.Record(ctx, charge); err != nil {
			t.Fatal(err)
		}
	}
	today := time.Date(2026, 11, 17, 0, 0, 0, 0, time.Local)
	if got, _ := ledger.Total(ctx, today, PurposeResearch); got.Yen != 20 {
		t.Errorf("今天的调查费用 = %v，应为 20", got.Yen)
	}
	if got, _ := ledger.Total(ctx, today, PurposeSearch, PurposeResearch); got.Credits != 2 {
		t.Errorf("今天的搜索积分 = %d，应为 2", got.Credits)
	}
	if got, _ := ledger.Total(ctx, today.AddDate(0, 0, -16)); got.Yen != 75 || got.Credits != 5 {
		t.Errorf("本月合计 = %+v，应为 75 日元 / 5 积分", got)
	}
}

func TestLedgerInMemory(t *testing.T) {
	ctx := context.Background()
	ledger, err := OpenLedger(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	_ = ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:x", Yen: 1.5})
	if got, _ := ledger.Total(ctx, time.Now().Add(-time.Minute)); got.Yen != 1.5 {
		t.Errorf("内存账本合计 = %v", got.Yen)
	}
}

func TestPricing(t *testing.T) {
	pricing := Pricing{USDToYen: 150, SearchCreditUSD: 0.008,
		Models: map[string]TokenPrice{"cloud": {InputUSDPerMillion: 0.5, OutputUSDPerMillion: 2}}}
	// 100 万输入 + 10 万输出 = 0.5 + 0.2 = 0.7 美元 = 105 日元
	if got := pricing.LLMYen("cloud", brain.ReplyUsage{PromptTokens: 1_000_000, OutputTokens: 100_000}); got < 104.99 || got > 105.01 {
		t.Errorf("云端费用 = %v，应为 105", got)
	}
	if got := pricing.LLMYen("local-8b", brain.ReplyUsage{PromptTokens: 1_000_000, OutputTokens: 100_000}); got != 0 {
		t.Errorf("本地模型应按 0 算：%v", got)
	}
	if got := pricing.SearchYen(10); got < 11.99 || got > 12.01 {
		t.Errorf("10 个积分 = %v，应为 12 日元", got)
	}
}

func TestGuardLimits(t *testing.T) {
	ctx := context.Background()
	ledger := openTestLedger(t)
	guard := NewGuard(ledger, Limits{ResearchDailyYen: 100, MonthlyYen: 1000, SearchDailyCredits: 30, WarnRatio: 0.8}, quietLogger)

	if err := guard.AllowResearch(ctx); err != nil {
		t.Fatalf("还没花钱，应允许调查：%v", err)
	}
	_ = ledger.Record(ctx, Charge{Purpose: PurposeResearch, Item: "llm:x", Credits: 28, Yen: 100})
	err := guard.AllowResearch(ctx)
	var spoken brain.SpokenError
	if !errors.Is(err, ErrOverBudget) || !errors.As(err, &spoken) {
		t.Fatalf("今天调查花满 100 日元应拒绝，并能念给主人：%v", err)
	}
	if key, values := spoken.Phrase(); key != brain.PhraseOverResearchDaily || values["spent"] != "100" || values["limit"] != "100" {
		t.Errorf("念给主人的那句和数：%s %v", key, values)
	}
	if err := guard.AllowSearch(ctx, 2); err != nil {
		t.Errorf("28+2 = 30 次，没超：%v", err)
	}
	if err := guard.AllowSearch(ctx, 3); !errors.Is(err, ErrOverBudget) {
		t.Errorf("28+3 > 30 次应拒绝：%v", err)
	}

	// 月度：用到 80% 提醒一次（说完那一轮之后不再附上），超过上限拒绝
	if briefing := guard.Brief("主人"); briefing.Lines != nil || briefing.Delivered != nil {
		t.Errorf("没到 80%% 不该提醒：%+v", briefing)
	}
	_ = ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:x", Yen: 750})
	if err := guard.AllowPaidLLM(ctx); err != nil {
		t.Fatalf("850 < 1000，应允许：%v", err)
	}
	briefing := guard.Brief("主人")
	if len(briefing.Lines) != 1 || !strings.Contains(briefing.Lines[0], "1000 日元") || briefing.Delivered == nil {
		t.Errorf("到 80%% 应提醒一次：%+v", briefing)
	}
	// 那一轮没说完（没确认）：下一轮照样附上
	if again := guard.Brief("主人"); len(again.Lines) != 1 {
		t.Errorf("没确认说过之前应一直附上：%+v", again)
	}
	briefing.Delivered()
	_ = guard.AllowPaidLLM(ctx)
	if again := guard.Brief("主人"); again.Lines != nil {
		t.Errorf("同一个月只提醒一次：%+v", again)
	}
	_ = ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:x", Yen: 200})
	if err := guard.AllowPaidLLM(ctx); !errors.Is(err, ErrOverBudget) {
		t.Errorf("超过月度上限应拒绝：%v", err)
	}
	if err := guard.AllowResearch(ctx); !errors.Is(err, ErrOverBudget) {
		t.Errorf("月度上限也管调查：%v", err)
	}
}

func TestGuardWithoutLimits(t *testing.T) {
	ctx := context.Background()
	ledger := openTestLedger(t)
	guard := NewGuard(ledger, Limits{}, quietLogger)
	_ = ledger.Record(ctx, Charge{Purpose: PurposeResearch, Item: "llm:x", Credits: 999, Yen: 99999})
	if guard.AllowResearch(ctx) != nil || guard.AllowSearch(ctx, 1) != nil || guard.AllowPaidLLM(ctx) != nil {
		t.Error("上限为 0 = 不限")
	}
}

// replyModel 回一句话，最后一段带用量。
type replyModel struct {
	text  string
	calls int
}

func (m *replyModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	m.calls++
	return func(yield func(brain.ReplyDelta, error) bool) {
		if !yield(brain.ReplyDelta{Text: m.text}, nil) {
			return
		}
		yield(brain.ReplyDelta{Usage: &brain.ReplyUsage{PromptTokens: 1_000_000, OutputTokens: 0}}, nil)
	}
}

func collectText(t *testing.T, model brain.ChatModel) (string, error) {
	t.Helper()
	var text strings.Builder
	for delta, err := range model.StreamReply(context.Background(), brain.ChatRequest{}) {
		if err != nil {
			return text.String(), err
		}
		text.WriteString(delta.Text)
	}
	return text.String(), nil
}

// v7：提示词缓存的读、写各按各的价；没配缓存价时按普通输入价（记多不记少）。
func TestPricingWithPromptCache(t *testing.T) {
	usage := brain.ReplyUsage{PromptTokens: 1_000_000, CacheReadTokens: 600_000, CacheWriteTokens: 300_000, OutputTokens: 100_000}
	priced := Pricing{USDToYen: 100, Models: map[string]TokenPrice{"claude": {InputUSDPerMillion: 0.10, OutputUSDPerMillion: 0.50,
		CacheReadUSDPerMillion: 0.01, CacheWriteUSDPerMillion: 0.125}}}
	// 没命中缓存 10 万 × 0.10 + 读 60 万 × 0.01 + 写 30 万 × 0.125 + 输出 10 万 × 0.50 = 0.01 + 0.006 + 0.0375 + 0.05 = 0.1035 美元
	if got := priced.LLMYen("claude", usage); got < 10.349 || got > 10.351 {
		t.Errorf("带缓存的费用 = %v，应为 10.35 日元", got)
	}
	unpriced := Pricing{USDToYen: 100, Models: map[string]TokenPrice{"claude": {InputUSDPerMillion: 0.10, OutputUSDPerMillion: 0.50}}}
	// 没配缓存价：100 万输入全按 0.10 + 输出 0.05 = 0.15 美元
	if got := unpriced.LLMYen("claude", usage); got < 14.99 || got > 15.01 {
		t.Errorf("没配缓存价时应按普通输入价算 = %v，应为 15 日元", got)
	}
}

// 写进留得久的缓存的那部分（CacheWriteLongTokens）按「那一档的输入价 × LongCacheWriteFactor」算，其余的缓存写照目录的价；
// 没填倍数时和一般的缓存写同价。
func TestPricingWithLongCacheWrites(t *testing.T) {
	usage := brain.ReplyUsage{PromptTokens: 1_000_000, CacheReadTokens: 600_000, CacheWriteTokens: 300_000,
		CacheWriteLongTokens: 200_000, OutputTokens: 100_000}
	price := TokenPrice{InputUSDPerMillion: 0.10, OutputUSDPerMillion: 0.50, CacheReadUSDPerMillion: 0.01,
		CacheWriteUSDPerMillion: 0.125, LongCacheWriteFactor: 2}
	priced := Pricing{USDToYen: 100, Models: map[string]TokenPrice{"claude": price}}
	// 没命中 10 万 × 0.10 + 读 60 万 × 0.01 + 一般的写 10 万 × 0.125 + 1 小时的写 20 万 × 0.20 + 输出 10 万 × 0.50
	// = 0.01 + 0.006 + 0.0125 + 0.04 + 0.05 = 0.1185 美元
	if got := priced.LLMYen("claude", usage); got < 11.849 || got > 11.851 {
		t.Errorf("带 1 小时缓存写的费用 = %v，应为 11.85 日元", got)
	}
	price.LongCacheWriteFactor = 0
	same := Pricing{USDToYen: 100, Models: map[string]TokenPrice{"claude": price}}
	if got := same.LLMYen("claude", usage); got < 10.349 || got > 10.351 {
		t.Errorf("没填倍数时和一般的缓存写同价 = %v，应为 10.35 日元", got)
	}
}

// v7：v7 以前建的账本（没有缓存两列）打开时自动补列，旧记录照常合计，新记录能写进缓存用量。
func TestLedgerUpgradesOldSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `CREATE TABLE charges (id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL,
		purpose TEXT NOT NULL, item TEXT NOT NULL, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		credits INTEGER NOT NULL DEFAULT 0, yen REAL NOT NULL DEFAULT 0);
		INSERT INTO charges (at, purpose, item, yen) VALUES (?, 'chat', 'llm:old', 7)`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	old.Close()

	ledger, err := OpenLedger(ctx, path)
	if err != nil {
		t.Fatalf("打开旧账本: %v", err)
	}
	if err := ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:new", InputTokens: 10, CacheReadTokens: 6,
		CacheWriteTokens: 3, Yen: 1}); err != nil {
		t.Fatalf("写入带缓存用量的记录: %v", err)
	}
	if got, _ := ledger.Total(ctx, time.Now().Add(-time.Minute)); got.Yen != 8 {
		t.Errorf("合计 = %v，应为 8（旧记录 7 + 新记录 1）", got.Yen)
	}
	ledger.Close()
	reopened, err := OpenLedger(ctx, path) // 再打开一次：列已经有了，不能重复加
	if err != nil {
		t.Fatalf("再次打开: %v", err)
	}
	reopened.Close()
}

func TestMeteredAndBudgetedModels(t *testing.T) {
	ctx := context.Background()
	ledger := openTestLedger(t)
	pricing := Pricing{USDToYen: 100, Models: map[string]TokenPrice{"cloud": {InputUSDPerMillion: 1}, "local": {Free: true}}}
	guard := NewGuard(ledger, Limits{MonthlyYen: 150}, quietLogger)
	cloud := &replyModel{text: "云端"}
	// 每次 100 万 token = 1 美元 = 100 日元
	model := NewBudgetedChatModel(NewMeteredChatModel(cloud, "cloud", PurposeChat, pricing, ledger, quietLogger), guard, quietLogger)

	for i := range 2 { // 0 → 100 → 200 日元
		if got, err := collectText(t, model); err != nil || got != "云端" {
			t.Errorf("第 %d 次：%q %v", i+1, got, err)
		}
	}
	if spent, _ := ledger.Total(ctx, time.Now().Add(-time.Minute)); spent.Yen != 200 {
		t.Errorf("账本合计 = %v，应为 200", spent.Yen)
	}
	// 第三次：已经超过 150 —— 不调用模型（不再改用本地顶替），交出能念给主人的错误
	_, err := collectText(t, model)
	var spoken brain.SpokenError
	if !errors.As(err, &spoken) || cloud.calls != 2 {
		t.Fatalf("超过上限应不调用、返回能念给主人的错误：%v（调用了 %d 次）", err, cloud.calls)
	}
	if key, values := spoken.Phrase(); key != brain.PhraseOverMonthlyBudget || values["limit"] == "" {
		t.Errorf("念给主人的那句和数：%s %v", key, values)
	}
}

// 就绪检查：账本写得进去时正常（内存账本和文件账本都是），检查不改数据。
func TestLedgerCheckWritable(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"", filepath.Join(t.TempDir(), "usage.db")} {
		ledger, err := OpenLedger(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.CheckWritable(ctx); err != nil {
			t.Errorf("账本 %q 应写得进去：%v", path, err)
		}
		if err := ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:test", Yen: 1}); err != nil {
			t.Errorf("检查之后应还能照常记账（写锁已放掉）：%v", err)
		}
		if totals, err := ledger.Total(ctx, time.Time{}); err != nil || totals.Yen != 1 {
			t.Errorf("检查不应改数据：%+v %v", totals, err)
		}
		ledger.Close()
	}
}

// 模型不在价格表里：记账模型建不起来（服务端起不来），不会悄悄按 0 日元记、让额度管不住它。
func TestMeteredRequiresPrice(t *testing.T) {
	pricing := Pricing{USDToYen: 100, Models: map[string]TokenPrice{"cloud": {InputUSDPerMillion: 1}, "local": {Free: true}}}
	for _, known := range []string{"cloud", "local"} {
		NewMeteredChatModel(&replyModel{text: "好"}, known, PurposeChat, pricing, openTestLedger(t), quietLogger)
	}
	defer func() {
		if recover() == nil {
			t.Error("不在价格表里的模型应建不起来")
		}
	}()
	NewMeteredChatModel(&replyModel{text: "好"}, "unknown", PurposeChat, pricing, openTestLedger(t), quietLogger)
}

// brokenLedger 读不出合计（库坏了、文件被锁住）。
type brokenLedger struct{ Ledger }

func (brokenLedger) Total(context.Context, time.Time, ...Purpose) (Totals, error) {
	return Totals{}, errors.New("database disk image is malformed")
}

// 读不出账本：不知道这个月花了多少，就不调用要花钱的模型，并把原因念给主人（以前照常放行，账本一坏花费就没有上限）。
func TestBudgetedRefusesWhenLedgerUnreadable(t *testing.T) {
	ledger := openTestLedger(t)
	guard := NewGuard(brokenLedger{ledger}, Limits{MonthlyYen: 3000}, quietLogger)
	cloud := &replyModel{text: "云端"}
	model := NewBudgetedChatModel(cloud, guard, quietLogger)
	_, err := collectText(t, model)
	var spoken brain.SpokenError
	if !errors.Is(err, ErrLedgerUnavailable) || !errors.As(err, &spoken) {
		t.Fatalf("应拒绝并给出念给主人的原因：%v", err)
	}
	if key, _ := spoken.Phrase(); key != brain.PhraseLedgerUnavailable {
		t.Errorf("念给主人的那句：%s", key)
	}
	if cloud.calls != 0 {
		t.Errorf("读不出账本时不应调用要花钱的模型：调用了 %d 次", cloud.calls)
	}
}

// 搜索记账：发出去、没拿到结果的请求照样记积分，标不完整；请求被打断（调用方的 ctx 已经取消）也照样记上。
func TestSearchMeterRecordsEveryRequest(t *testing.T) {
	ledger := openTestLedger(t)
	meter := NewSearchMeter(NewGuard(ledger, Limits{}, quietLogger), ledger, Pricing{USDToYen: 150, SearchCreditUSD: 0.01},
		PurposeSearch, "tavily")
	interrupted, cancel := context.WithCancel(context.Background())
	cancel()
	if err := meter.RecordSearch(interrupted, 1, true); err != nil {
		t.Fatalf("被打断的请求也要记上：%v", err)
	}
	if err := meter.RecordSearch(context.Background(), 2, false); err != nil {
		t.Fatal(err)
	}
	rows, err := ledger.(*sqliteLedger).db.Query(`SELECT credits, incomplete FROM charges ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type charge struct {
		credits    int
		incomplete bool
	}
	var got []charge
	for rows.Next() {
		var c charge
		if err := rows.Scan(&c.credits, &c.incomplete); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	if want := []charge{{1, true}, {2, false}}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("每次请求一笔、没拿到结果的标不完整：%+v", got)
	}
	if totals, err := ledger.Total(context.Background(), time.Time{}); err != nil || totals.Credits != 3 || totals.Yen != 4.5 {
		t.Errorf("不完整的那笔照样算进额度：%+v %v", totals, err)
	}
}
