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
	guard := NewGuard(ledger, Limits{Currency: "JPY", Monthly: jpy(100), MonthlyRaise: jpy(50)}, fixedRates{}, quietLogger)
	guard.now = func() time.Time { return november }
	if err := ledger.Record(ctx, Charge{At: november, Purpose: PurposeChat, Item: "llm:cloud", Cost: jpy(120)}); err != nil {
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
	limit, err := guard.RaiseMonthlyCap(confirmedRaise("主人"), "主人")
	if err != nil || limit != jpy(150) {
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
	guard = NewGuard(reopened, Limits{Currency: "JPY", Monthly: jpy(100), MonthlyRaise: jpy(50)}, fixedRates{}, quietLogger)
	guard.now = func() time.Time { return november }
	if err := guard.AllowPaidLLM(ctx); err != nil {
		t.Errorf("重开账本后提高的照样算：%v", err)
	}

	december := time.Date(2026, 12, 2, 12, 0, 0, 0, time.Local)
	guard.now = func() time.Time { return december }
	if err := reopened.Record(ctx, Charge{At: december, Purpose: PurposeChat, Item: "llm:cloud", Cost: jpy(120)}); err != nil {
		t.Fatal(err)
	}
	if err := guard.AllowPaidLLM(ctx); !errors.Is(err, brain.ErrMonthlyCapReached) {
		t.Errorf("下个月回到配置的上限（100），花了 120 应到上限：%v", err)
	}
}

// confirmedRaise 是带着 person 对提高上限的确认的 ctx（确认闸执行时就是这样带的）。
func confirmedRaise(person string) context.Context {
	return brain.WithConfirmation(context.Background(), brain.Confirmation{ActionID: "act_1", Tool: brain.RaiseMonthlyCapTool, Person: person})
}

// 提高上限由额度那一层自己守门：绕过会话直接调用、带着别的工具的确认、确认的不是这个人，都不提高（账本里没有记录）；
// 阳性对照：带着这个人对提高上限的确认就提高。
func TestMonthlyCapRaiseNeedsConfirmation(t *testing.T) {
	ctx := context.Background()
	ledger := openTestLedger(t)
	guard := NewGuard(ledger, Limits{Currency: "JPY", Monthly: jpy(100), MonthlyRaise: jpy(50)}, fixedRates{}, quietLogger)
	for label, call := range map[string]context.Context{
		"绕过会话直接调用":  ctx,
		"带着别的工具的确认": brain.WithConfirmation(ctx, brain.Confirmation{ActionID: "act_1", Tool: "delete_note", Person: "主人"}),
		"确认的不是这个人":  confirmedRaise("家人"),
	} {
		if _, err := guard.RaiseMonthlyCap(call, "主人"); !errors.Is(err, brain.ErrNotConfirmed) {
			t.Errorf("%s：应拒绝：%v", label, err)
		}
	}
	if raised, err := ledger.CapRaised(ctx, time.Time{}); err != nil || raised.Amount("JPY").Micros != 0 {
		t.Fatalf("没确认的不该提高：%v %v", raised, err)
	}
	if limit, err := guard.RaiseMonthlyCap(confirmedRaise("主人"), "主人"); err != nil || limit != jpy(150) {
		t.Errorf("带着主人的确认应能提高：%v %v", limit, err)
	}
}

// 不允许口头提高：念的那句不提提高，提高的调用报错；别的额度（搜索每天的次数）不算月度上限。
func TestMonthlyCapWithoutRaise(t *testing.T) {
	ctx := context.Background()
	ledger := openTestLedger(t)
	guard := NewGuard(ledger, Limits{Currency: "JPY", Monthly: jpy(100), SearchDailyCredits: 1}, fixedRates{}, quietLogger)
	if err := ledger.Record(ctx, Charge{Purpose: PurposeSearch, Item: "tavily", Credits: 1, Cost: jpy(120)}); err != nil {
		t.Fatal(err)
	}
	var spoken brain.SpokenError
	if err := guard.AllowPaidLLM(ctx); !errors.As(err, &spoken) {
		t.Fatalf("应到上限：%v", err)
	} else if key, _ := spoken.Phrase(); key != brain.PhraseOverMonthlyBudget {
		t.Errorf("不能口头提高时念原来那句：%s", key)
	}
	if _, err := guard.RaiseMonthlyCap(confirmedRaise("主人"), "主人"); err == nil || errors.Is(err, brain.ErrNotConfirmed) {
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

// 代理定义里写的记账用途：认识的换成 Purpose，写错的报错。
func TestParsePurpose(t *testing.T) {
	for _, name := range []string{"chat", "strong", "vision", "summary", "research", "search"} {
		if purpose, err := ParsePurpose(name); err != nil || string(purpose) != name {
			t.Errorf("%s：%v", name, err)
		}
	}
	if _, err := ParsePurpose("chatt"); err == nil {
		t.Error("写错的用途应报错")
	}
}
