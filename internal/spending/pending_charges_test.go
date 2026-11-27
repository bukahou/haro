package spending

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

var errDiskFull = errors.New("database or disk is full")

// flakyLedger 读得出，写可以被弄坏（像数据卷写满时那样）。
type flakyLedger struct {
	Ledger
	failing atomic.Bool
}

func (f *flakyLedger) Record(ctx context.Context, charge Charge) error {
	if f.failing.Load() {
		return errDiskFull
	}
	return f.Ledger.Record(ctx, charge)
}

// 写不进账本时：那一笔留在内存里；补写成功之前，有上限的付费调用、调查、搜索都不放行（念「账本用不了」那句），
// 就绪检查也报写不进；写得进了，下一次放行前补写，自动恢复，补写的那笔算在合计里。
func TestUnwrittenChargesStopPaidCallsUntilWritten(t *testing.T) {
	ctx := context.Background()
	flaky := &flakyLedger{Ledger: openTestLedger(t)}
	ledger := WithBookkeeping(flaky, quietLogger)
	guard := NewGuard(ledger, Limits{Currency: "JPY", Monthly: jpy(1000), ResearchDaily: jpy(500), SearchDailyCredits: 30}, fixedRates{}, quietLogger)
	pricing := Pricing{Models: map[string]TokenPrice{"cloud": {InputUSDPerMillion: 1}}}
	cloud := &replyModel{text: "云端"} // 每次 100 万 token = 100 日元
	model := NewBudgetedChatModel(NewMeteredChatModel(cloud, "cloud", PurposeChat, pricing, ledger, quietLogger), guard, quietLogger)

	if _, err := collectText(t, model); err != nil {
		t.Fatal(err)
	}
	flaky.failing.Store(true)
	if _, err := collectText(t, model); err != nil { // 放行时还写得进：照常回答；这次的用量写不进
		t.Fatalf("放行时账本还没出问题：应照常回答：%v", err)
	}
	_, err := collectText(t, model)
	var spoken brain.SpokenError
	if !errors.Is(err, ErrLedgerUnavailable) || !errors.As(err, &spoken) || cloud.calls != 2 {
		t.Fatalf("有写不进的用量时不应再调用要花钱的模型：%v（调用了 %d 次）", err, cloud.calls)
	}
	if key, _ := spoken.Phrase(); key != brain.PhraseLedgerUnavailable {
		t.Errorf("念给主人的那句：%s", key)
	}
	if err := guard.AllowResearch(ctx); !errors.Is(err, ErrLedgerUnavailable) {
		t.Errorf("调查也不放行：%v", err)
	}
	if err := guard.AllowSearch(ctx, 1); !errors.Is(err, ErrLedgerUnavailable) {
		t.Errorf("搜索也不放行：%v", err)
	}
	if err := ledger.CheckWritable(ctx); !errors.Is(err, errDiskFull) {
		t.Errorf("就绪检查应报写不进：%v", err)
	}

	flaky.failing.Store(false)
	if got, err := collectText(t, model); err != nil || got != "云端" {
		t.Fatalf("写得进了：补写后照常回答：%q %v", got, err)
	}
	if spent, err := flaky.Total(ctx, time.Time{}); err != nil || spentJPY(spent) != 300 {
		t.Errorf("写不进的那笔补写后应算在合计里（3 次 × 100 日元）：%+v %v", spent, err)
	}
	if err := ledger.CheckWritable(ctx); err != nil {
		t.Errorf("恢复后就绪检查应正常：%v", err)
	}
}

// 补写时按发生的时间记：昨天写不进的那笔算在昨天，不算进今天的额度。
func TestUnwrittenChargeKeepsItsTime(t *testing.T) {
	ctx := context.Background()
	flaky := &flakyLedger{Ledger: openTestLedger(t)}
	ledger := WithBookkeeping(flaky, quietLogger)
	today := time.Date(2026, 11, 21, 0, 0, 0, 0, time.Local)
	ledger.(*pendingChargesLedger).now = func() time.Time { return today.Add(-time.Hour) }

	flaky.failing.Store(true)
	if err := ledger.Record(ctx, Charge{Purpose: PurposeResearch, Item: "llm:cloud", Cost: jpy(5)}); !errors.Is(err, errDiskFull) {
		t.Fatalf("写不进时应把原因交给调用方：%v", err)
	}
	flaky.failing.Store(false)
	if err := ledger.FlushPending(ctx); err != nil {
		t.Fatal(err)
	}
	if spent, _ := flaky.Total(ctx, today, PurposeResearch); spentJPY(spent) != 0 {
		t.Errorf("昨天的那笔不应算进今天：%v", spentJPY(spent))
	}
	if spent, _ := flaky.Total(ctx, today.AddDate(0, 0, -1), PurposeResearch); spentJPY(spent) != 5 {
		t.Errorf("昨天的那笔应算在昨天：%v", spentJPY(spent))
	}
}

// 内存里最多留 maxPendingCharges 笔：满了以后再来的记不上（报错），已经留着的照样补写。
func TestUnwrittenChargesAreBounded(t *testing.T) {
	ctx := context.Background()
	flaky := &flakyLedger{Ledger: openTestLedger(t)}
	ledger := WithBookkeeping(flaky, quietLogger)
	flaky.failing.Store(true)
	for range maxPendingCharges + 1 {
		if err := ledger.Record(ctx, Charge{Purpose: PurposeSearch, Item: "tavily", Credits: 1}); err == nil {
			t.Fatal("写不进时应报错")
		}
	}
	if kept := len(ledger.(*pendingChargesLedger).pending); kept != maxPendingCharges {
		t.Errorf("内存里应只留 %d 笔：%d", maxPendingCharges, kept)
	}
	flaky.failing.Store(false)
	if err := ledger.FlushPending(ctx); err != nil {
		t.Fatal(err)
	}
	if used, _ := flaky.Total(ctx, time.Time{}); used.Credits != maxPendingCharges {
		t.Errorf("留着的都应补写上：%d 笔", used.Credits)
	}
}

// 下一次记账之前先补写（按先后）；关停前最后补写一次，补写不了就打日志说明丢了几笔。
// 日志：开始写不进时一行（错误级别），补写上了一行（写了几笔），补写上的每一笔照样有记账的那一行。
func TestUnwrittenChargesWrittenOnNextRecordAndClose(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	inner, err := OpenLedger(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyLedger{Ledger: inner}
	logs := &logRecords{}
	ledger := WithBookkeeping(flaky, slog.New(logs))

	flaky.failing.Store(true)
	_ = ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:first", Cost: jpy(1)})
	_ = ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:second", Cost: jpy(2)})
	if outage, levels := logs.withField("err"); len(outage) != 1 || levels[0] != slog.LevelError {
		t.Errorf("开始写不进时应打一行错误日志（不是每笔都打）：%v %v", outage, levels)
	}
	flaky.failing.Store(false)
	if err := ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:third", Cost: jpy(4)}); err != nil {
		t.Fatalf("写得进了：%v", err)
	}
	if spent, _ := flaky.Total(ctx, time.Time{}); spentJPY(spent) != 7 {
		t.Errorf("记下一笔之前应先补写：%v", spentJPY(spent))
	}
	if recovered, _ := logs.withField("written"); len(recovered) != 1 || recovered[0]["written"] != int64(2) {
		t.Errorf("补写上了应打一行，写明几笔：%v", recovered)
	}
	if charged, _ := logs.withField("item"); len(charged) != 3 || charged[0]["item"] != "llm:first" || charged[2]["item"] != "llm:third" {
		t.Errorf("补写上的每一笔照样有记账的那一行（按先后）：%v", charged)
	}

	flaky.failing.Store(true)
	_ = ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:at-close", Cost: jpy(8)})
	flaky.failing.Store(false)
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if spent, _ := reopened.Total(ctx, time.Time{}); spentJPY(spent) != 15 {
		t.Errorf("关停前应最后补写一次：%v", spentJPY(spent))
	}
}

// 关停时还写不进：丢掉的几笔、多少钱打进日志（不悄悄丢）。
func TestUnwrittenChargesLostAtCloseAreLogged(t *testing.T) {
	ctx := context.Background()
	flaky := &flakyLedger{Ledger: openTestLedger(t)}
	logs := &logRecords{}
	ledger := WithBookkeeping(flaky, slog.New(logs))
	flaky.failing.Store(true)
	_ = ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:x", Cost: jpy(3)})
	_ = ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:x", Cost: jpy(4)})
	_ = ledger.Close()
	lost, levels := logs.withField("charges")
	if len(lost) != 1 || lost[0]["charges"] != int64(2) || lost[0]["cost"] != "7 JPY" || levels[0] != slog.LevelError {
		t.Errorf("丢掉的几笔、多少钱要打一行错误日志：%v %v", lost, levels)
	}
}
