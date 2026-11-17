package spending

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"haro/internal/brain"
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
	if got := pricing.LLMYen("cloud", 1_000_000, 100_000); got < 104.99 || got > 105.01 {
		t.Errorf("云端费用 = %v，应为 105", got)
	}
	if got := pricing.LLMYen("local-8b", 1_000_000, 100_000); got != 0 {
		t.Errorf("本地模型应按 0 算：%v", got)
	}
	if got := pricing.SearchYen(10); got < 11.99 || got > 12.01 {
		t.Errorf("10 个积分 = %v，应为 12 日元", got)
	}
}

func TestGuardLimits(t *testing.T) {
	ctx := context.Background()
	ledger := openTestLedger(t)
	guard := NewGuard(ledger, Limits{ResearchDailyYen: 100, LLMMonthlyYen: 1000, SearchDailyCredits: 30, WarnRatio: 0.8}, quietLogger)

	if err := guard.AllowResearch(ctx); err != nil {
		t.Fatalf("还没花钱，应允许调查：%v", err)
	}
	_ = ledger.Record(ctx, Charge{Purpose: PurposeResearch, Item: "llm:x", Credits: 28, Yen: 100})
	err := guard.AllowResearch(ctx)
	var spoken brain.SpokenError
	if !errors.Is(err, ErrOverBudget) || !errors.As(err, &spoken) || !strings.Contains(spoken.SpokenMessage(), "100 日元") {
		t.Errorf("今天调查花满 100 日元应拒绝，并能念给主人：%v", err)
	}
	if err := guard.AllowSearch(ctx, 2); err != nil {
		t.Errorf("28+2 = 30 次，没超：%v", err)
	}
	if err := guard.AllowSearch(ctx, 3); !errors.Is(err, ErrOverBudget) {
		t.Errorf("28+3 > 30 次应拒绝：%v", err)
	}

	// 月度：用到 80% 提醒一次（只进一轮提示词），超过上限拒绝
	if lines := guard.Brief("主人"); lines != nil {
		t.Errorf("没到 80%% 不该提醒：%v", lines)
	}
	_ = ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:x", Yen: 750})
	if err := guard.AllowPaidLLM(ctx); err != nil {
		t.Fatalf("850 < 1000，应允许：%v", err)
	}
	if lines := guard.Brief("主人"); len(lines) != 1 || !strings.Contains(lines[0], "1000 日元") {
		t.Errorf("到 80%% 应提醒一次：%v", lines)
	}
	_ = guard.AllowPaidLLM(ctx)
	if lines := guard.Brief("主人"); lines != nil {
		t.Errorf("同一个月只提醒一次：%v", lines)
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

func (m *replyModel) StreamReply(context.Context, []brain.ChatMessage, []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	m.calls++
	return func(yield func(brain.ReplyDelta, error) bool) {
		if !yield(brain.ReplyDelta{Text: m.text}, nil) {
			return
		}
		yield(brain.ReplyDelta{Usage: &brain.ReplyUsage{PromptTokens: 1_000_000, OutputTokens: 0}}, nil)
	}
}

func (m *replyModel) Warmup(context.Context) error { return nil }

func collectText(t *testing.T, model brain.ChatModel) (string, error) {
	t.Helper()
	var text strings.Builder
	for delta, err := range model.StreamReply(context.Background(), nil, nil) {
		if err != nil {
			return text.String(), err
		}
		text.WriteString(delta.Text)
	}
	return text.String(), nil
}

func TestMeteredAndBudgetedModels(t *testing.T) {
	ctx := context.Background()
	ledger := openTestLedger(t)
	pricing := Pricing{USDToYen: 100, Models: map[string]TokenPrice{"cloud": {InputUSDPerMillion: 1}}}
	guard := NewGuard(ledger, Limits{LLMMonthlyYen: 150}, quietLogger)
	cloud, local := &replyModel{text: "云端"}, &replyModel{text: "本地"}
	model := NewBudgetedChatModel(
		NewMeteredChatModel(cloud, "cloud", PurposeChat, pricing, ledger, quietLogger), // 每次 100 万 token = 1 美元 = 100 日元
		NewMeteredChatModel(local, "local", PurposeChat, pricing, ledger, quietLogger), guard, quietLogger)

	for i, want := range []string{"云端", "云端", "本地"} { // 0 → 100 → 200 日元：第三次超过 150，改用本地
		if got, err := collectText(t, model); err != nil || got != want {
			t.Errorf("第 %d 次：%q %v，应为 %q", i+1, got, err, want)
		}
	}
	if spent, _ := ledger.Total(ctx, time.Now().Add(-time.Minute)); spent.Yen != 200 {
		t.Errorf("账本合计 = %v，应为 200（本地模型按 0 记）", spent.Yen)
	}

	noFallback := NewBudgetedChatModel(cloud, nil, guard, quietLogger)
	_, err := collectText(t, noFallback)
	var spoken brain.SpokenError
	if !errors.As(err, &spoken) || !strings.Contains(spoken.SpokenMessage(), "上限") {
		t.Errorf("没有本地退路时应返回能念给主人的错误：%v", err)
	}
}
