package spending

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/money"
)

// testAccountOf 是测试里的「人 → 账号」：「acct:<编号>」的人是那个账号，别的都是管理员的（空）。
func testAccountOf(person string) string {
	id, _ := strings.CutPrefix(person, "acct:")
	if id == person {
		return ""
	}
	return id
}

// fakeCaps 是每个账号的月上限。
type fakeCaps map[string]money.Amount

func (c fakeCaps) MonthlyCapOf(_ context.Context, account string) (money.Amount, error) {
	return c[account], nil
}

func forPerson(person string) context.Context {
	return brain.WithBillingPerson(context.Background(), person)
}

// 记账按 ctx 里标的人记上账号（没标的、管理员的人记成空）；按账号合计只算那个账号，全部合计照旧是所有账号一起。
func TestAttribution(t *testing.T) {
	inner, err := OpenLedger(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	ledger := WithAttribution(inner, testAccountOf)
	now := time.Now()
	for person, cost := range map[string]float64{"acct:a": 10, "acct:b": 20, "主人": 30, "": 40} {
		if err := ledger.Record(forPerson(person), Charge{At: now, Purpose: PurposeChat, Item: "llm:x", Cost: jpy(cost)}); err != nil {
			t.Fatal(err)
		}
	}
	since := now.Add(-time.Minute)
	for account, want := range map[string]float64{"a": 10, "b": 20, "": 70} {
		if got, err := ledger.AccountTotal(context.Background(), account, since); err != nil || spentJPY(got) != want {
			t.Errorf("账号 %q：%v %v", account, spentJPY(got), err)
		}
	}
	if got, _ := ledger.Total(context.Background(), since); spentJPY(got) != 100 {
		t.Errorf("全部：%v", spentJPY(got))
	}
}

// 账号到了自己的月上限：这个账号不放行（念「管理员定的上限」，不进口头提高），别的账号、管理员照常；
// 汇总按账号给（这个账号自己的、它的上限）。
func TestAccountMonthlyCap(t *testing.T) {
	inner, err := OpenLedger(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	ledger := WithAttribution(inner, testAccountOf)
	november := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	guard := NewGuard(ledger, Limits{Currency: "JPY", Monthly: jpy(1000), MonthlyRaise: jpy(50)}, fixedRates{}, quietLogger)
	guard.now = func() time.Time { return november }
	guard.UseAccounts(testAccountOf, fakeCaps{"a": jpy(100)})
	if err := ledger.Record(forPerson("acct:a"), Charge{At: november, Purpose: PurposeChat, Item: "llm:x", Cost: jpy(120)}); err != nil {
		t.Fatal(err)
	}
	err = guard.AllowPaidLLM(forPerson("acct:a"))
	var spoken brain.SpokenError
	if !errors.Is(err, ErrOverBudget) || errors.Is(err, brain.ErrMonthlyCapReached) || !errors.As(err, &spoken) {
		t.Fatalf("账号到了上限：%v", err)
	}
	if key, values := spoken.Phrase(); key != brain.PhraseOverAccountMonthly || values["limit"] != "100" || values["spent"] != "120" {
		t.Errorf("念的那句：%s %v", key, values)
	}
	for _, person := range []string{"acct:b", "主人"} {
		if err := guard.AllowPaidLLM(forPerson(person)); err != nil {
			t.Errorf("%s 不受影响：%v", person, err)
		}
	}
	summary, err := guard.SummaryFor(context.Background(), "a")
	if err != nil || summary.Status != StatusReached || summary.MonthCap != jpy(100) || summary.Month.Total.Amount != jpy(120) || summary.CanRaise {
		t.Errorf("账号的汇总：%+v %v", summary, err)
	}
	if spent, _ := guard.AccountSpent(context.Background(), ""); spent.Total.Amount.Micros != 0 {
		t.Errorf("管理员自己没花：%+v", spent)
	}
	if global, _ := guard.SummaryFor(context.Background(), ""); global.Month.Total.Amount != jpy(120) || !global.CanRaise {
		t.Errorf("管理员看全局的：%+v", global)
	}
}

// 全局的上限到了：管理员听到能口头提高；别的账号只听到到了上限（不进「要不要提高」），也提高不了；
// 全局的额度提醒只告诉管理员。
func TestGlobalCapIsAdminOnly(t *testing.T) {
	inner, err := OpenLedger(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	ledger := WithAttribution(inner, testAccountOf)
	november := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	guard := NewGuard(ledger, Limits{Currency: "JPY", Monthly: jpy(100), MonthlyRaise: jpy(50), WarnRatio: 0.5}, fixedRates{}, quietLogger)
	guard.now = func() time.Time { return november }
	guard.UseAccounts(testAccountOf, fakeCaps{})
	if err := ledger.Record(forPerson("acct:a"), Charge{At: november, Purpose: PurposeChat, Item: "llm:x", Cost: jpy(60)}); err != nil {
		t.Fatal(err)
	}
	if err := guard.AllowPaidLLM(forPerson("acct:a")); err != nil {
		t.Fatal(err)
	}
	if brief := guard.Brief("acct:a"); len(brief.Lines) != 0 {
		t.Errorf("别的账号不听全局的提醒：%v", brief.Lines)
	}
	if brief := guard.Brief("主人"); len(brief.Lines) != 1 {
		t.Errorf("管理员听提醒：%v", brief.Lines)
	}
	if err := ledger.Record(forPerson("acct:a"), Charge{At: november, Purpose: PurposeChat, Item: "llm:x", Cost: jpy(60)}); err != nil {
		t.Fatal(err)
	}
	var spoken brain.SpokenError
	if err := guard.AllowPaidLLM(forPerson("acct:a")); errors.Is(err, brain.ErrMonthlyCapReached) || !errors.As(err, &spoken) {
		t.Errorf("别的账号：%v", err)
	} else if key, _ := spoken.Phrase(); key != brain.PhraseOverMonthlyBudget {
		t.Errorf("别的账号念的：%s", key)
	}
	if err := guard.AllowPaidLLM(forPerson("主人")); !errors.Is(err, brain.ErrMonthlyCapReached) {
		t.Errorf("管理员能提高：%v", err)
	}
	if _, err := guard.RaiseMonthlyCap(confirmedRaise("acct:a"), "acct:a"); err == nil {
		t.Error("别的账号不能提高全局的上限")
	}
}
