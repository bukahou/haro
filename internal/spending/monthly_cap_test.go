package spending

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// 到上限：交出能认出是月度上限、能念给主人的错误（说怎么提高）；主人确认提高后放行；
// 提高的记在账本里，重开账本照样算；下个月回到配置的上限。
func TestMonthlyCapRaise(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	ledger, err := OpenLedger(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	november := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	guard := NewGuard(ledger, Limits{MonthlyYen: 100, MonthlyRaiseYen: 50}, quietLogger)
	guard.now = func() time.Time { return november }
	if err := ledger.Record(ctx, Charge{At: november, Purpose: PurposeChat, Item: "llm:cloud", Yen: 120}); err != nil {
		t.Fatal(err)
	}

	err = guard.AllowPaidLLM(ctx)
	var spoken brain.SpokenError
	if !errors.Is(err, brain.ErrMonthlyCapReached) || !errors.Is(err, ErrOverBudget) || !errors.As(err, &spoken) {
		t.Fatalf("到上限应能认出是月度上限、能念给主人：%v", err)
	}
	if key, values := spoken.Phrase(); key != brain.PhraseOverMonthlyBudgetRaisable || values["raise"] != "50" || values["limit"] != "100" {
		t.Errorf("念的那句要说能提高多少：%s %v", key, values)
	}
	limit, err := guard.RaiseMonthlyCap(ctx, "主人")
	if err != nil || limit != 150 {
		t.Fatalf("提高后上限应为 150：%v %v", limit, err)
	}
	if err := guard.AllowPaidLLM(ctx); err != nil {
		t.Errorf("提高后应放行：%v", err)
	}
	ledger.Close()

	reopened, err := OpenLedger(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	guard = NewGuard(reopened, Limits{MonthlyYen: 100, MonthlyRaiseYen: 50}, quietLogger)
	guard.now = func() time.Time { return november }
	if err := guard.AllowPaidLLM(ctx); err != nil {
		t.Errorf("重开账本后提高的照样算：%v", err)
	}

	december := time.Date(2026, 12, 2, 12, 0, 0, 0, time.Local)
	guard.now = func() time.Time { return december }
	if err := reopened.Record(ctx, Charge{At: december, Purpose: PurposeChat, Item: "llm:cloud", Yen: 120}); err != nil {
		t.Fatal(err)
	}
	if err := guard.AllowPaidLLM(ctx); !errors.Is(err, brain.ErrMonthlyCapReached) {
		t.Errorf("下个月回到配置的上限（100），花了 120 应到上限：%v", err)
	}
}

// 不允许口头提高：念的那句不提提高，提高的调用报错；别的额度（搜索每天的次数）不算月度上限。
func TestMonthlyCapWithoutRaise(t *testing.T) {
	ctx := context.Background()
	ledger := openTestLedger(t)
	guard := NewGuard(ledger, Limits{MonthlyYen: 100, SearchDailyCredits: 1}, quietLogger)
	if err := ledger.Record(ctx, Charge{Purpose: PurposeSearch, Item: "tavily", Credits: 1, Yen: 120}); err != nil {
		t.Fatal(err)
	}
	var spoken brain.SpokenError
	if err := guard.AllowPaidLLM(ctx); !errors.As(err, &spoken) {
		t.Fatalf("应到上限：%v", err)
	} else if key, _ := spoken.Phrase(); key != brain.PhraseOverMonthlyBudget {
		t.Errorf("不能口头提高时念原来那句：%s", key)
	}
	if _, err := guard.RaiseMonthlyCap(ctx, "主人"); err == nil {
		t.Error("不允许口头提高时应报错")
	}
	if err := guard.AllowSearch(ctx, 1); err == nil || errors.Is(err, brain.ErrMonthlyCapReached) {
		t.Errorf("搜索每天的次数到了：是额度用完，但不是月度上限：%v", err)
	}
}

// 不受额度管的那次小调用按天限次数：用完了不调用，第二天重新算。
func TestDailyCallLimit(t *testing.T) {
	inner := &replyModel{text: "好"}
	limited := NewDailyCallLimit(inner, 2)
	day := time.Date(2026, 11, 21, 9, 0, 0, 0, time.Local)
	limited.now = func() time.Time { return day }
	for i := range 2 {
		if _, err := collectText(t, limited); err != nil {
			t.Fatalf("第 %d 次应放行：%v", i+1, err)
		}
	}
	if _, err := collectText(t, limited); !errors.Is(err, ErrOverBudget) || inner.calls != 2 {
		t.Errorf("第 3 次应不调用：%v（调用了 %d 次）", err, inner.calls)
	}
	day = day.Add(24 * time.Hour)
	if _, err := collectText(t, limited); err != nil || inner.calls != 3 {
		t.Errorf("第二天重新算：%v（调用了 %d 次）", err, inner.calls)
	}
}
