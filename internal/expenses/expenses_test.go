package expenses

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"iter"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/money"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeParser 交回固定的草稿；unreadable 时报看不清。
type fakeParser struct{ unreadable bool }

func (f fakeParser) Parse(_ context.Context, image []byte, _ []Category, hint Hint) (Draft, error) {
	if f.unreadable || len(image) == 0 {
		return Draft{}, ErrUnreadable
	}
	spent := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	return Draft{Merchant: "ロピア", SpentAt: &spent, Currency: "JPY", Total: "2300", Tax: "170", Category: hint.Category,
		Items:     []DraftItem{{Name: "白菜", Amount: "198"}, {Name: "豚肉", Quantity: "2", UnitPrice: "551", Amount: "1102", TaxRate: "8"}},
		Uncertain: []string{"payment_method"}}, nil
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 30, 60))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func newService(t *testing.T, parser Parser) (*Service, *attachments.Files) {
	t.Helper()
	store, err := OpenStore(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	files, err := attachments.OpenFiles(filepath.Join(t.TempDir(), "images"))
	if err != nil {
		t.Fatal(err)
	}
	return NewService(context.Background(), Options{Store: store, Images: files, Parser: parser, Location: time.UTC, Logger: quiet}), files
}

func waitParsed(t *testing.T, service *Service, person, id string) Receipt {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := service.Receipt(context.Background(), person, id)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != StatusParsing {
			return receipt
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("一直在解析")
	return Receipt{}
}

// 交小票 → 后台解析成草稿 → 主人改了再确认 → 入账（带原图）；同一张不能确认两次；别人看不到；删账目时原图跟着删。
func TestReceiptToExpense(t *testing.T) {
	service, files := newService(t, fakeParser{})
	ctx := context.Background()
	receipt, err := service.Upload(ctx, "主人", pngBytes(t), Hint{Category: "groceries", Note: "周末买菜"})
	if err != nil || receipt.Status != StatusParsing {
		t.Fatalf("交小票：%+v %v", receipt, err)
	}
	parsed := waitParsed(t, service, "主人", receipt.ID)
	if parsed.Status != StatusReady || parsed.Draft.Merchant != "ロピア" || parsed.Draft.Category != "groceries" || len(parsed.Draft.Items) != 2 {
		t.Fatalf("草稿：%+v", parsed)
	}
	draft := *parsed.Draft
	draft.Total, draft.PaymentMethod = "2310", "PayPay"
	expense, err := service.Confirm(ctx, "主人", receipt.ID, draft)
	if err != nil || expense.Total.Micros != 2310*money.MicrosPerUnit || expense.AttachmentID == "" || expense.Items[1].UnitPrice.Micros != 551*money.MicrosPerUnit {
		t.Fatalf("入账：%+v %v", expense, err)
	}
	if _, err := service.Confirm(ctx, "主人", receipt.ID, draft); !errors.Is(err, ErrState) {
		t.Errorf("确认两次：%v", err)
	}
	if _, err := service.Expense(ctx, "别人", expense.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("别人的：%v", err)
	}
	if _, err := service.ExpenseImage(ctx, "主人", expense.ID); err != nil {
		t.Errorf("原图：%v", err)
	}
	if err := service.Delete(ctx, "主人", expense.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Read(expense.AttachmentID); err == nil {
		t.Error("删账目时原图跟着删")
	}
}

// 看不清的：解析失败（原因 unreadable），不能确认；分类不对的不收。
func TestUnreadableAndValidation(t *testing.T) {
	service, _ := newService(t, fakeParser{unreadable: true})
	ctx := context.Background()
	receipt, err := service.Upload(ctx, "主人", pngBytes(t), Hint{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed := waitParsed(t, service, "主人", receipt.ID); parsed.Status != StatusFailed || parsed.Failure != FailureUnreadable {
		t.Errorf("看不清：%+v", parsed)
	}
	if _, err := service.Upload(ctx, "主人", pngBytes(t), Hint{Category: "不存在"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("没有这个分类：%v", err)
	}
	if _, err := service.Upload(ctx, "主人", []byte("不是图片"), Hint{}); !errors.Is(err, attachments.ErrUnsupported) {
		t.Errorf("不是图片：%v", err)
	}
	spent := time.Now()
	for name, draft := range map[string]Draft{
		"没店名":   {SpentAt: &spent, Currency: "JPY", Total: "100"},
		"没时间":   {Merchant: "店", Currency: "JPY", Total: "100"},
		"合计不对":  {Merchant: "店", SpentAt: &spent, Currency: "JPY", Total: "一百"},
		"币种不对":  {Merchant: "店", SpentAt: &spent, Currency: "YEN", Total: "100"},
		"明细没名":  {Merchant: "店", SpentAt: &spent, Currency: "JPY", Total: "100", Items: []DraftItem{{Amount: "100"}}},
		"税率不是数": {Merchant: "店", SpentAt: &spent, Currency: "JPY", Total: "100", Items: []DraftItem{{Name: "a", Amount: "100", TaxRate: "八"}}},
	} {
		if _, err := service.Create(ctx, "主人", draft); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s：%v", name, err)
		}
	}
}

// 自己加的分类能用、能删；按月汇总：按币种分开、按分类和店分组、上个月的合计。
func TestCategoriesAndSummary(t *testing.T) {
	service, _ := newService(t, fakeParser{})
	ctx := context.Background()
	pets, err := service.AddCategory(ctx, "主人", "猫")
	if err != nil {
		t.Fatal(err)
	}
	if categories, _ := service.Categories(ctx, "主人"); len(categories) != len(BuiltinCategories)+1 || categories[len(categories)-1].Name != "猫" {
		t.Errorf("分类：%+v", categories)
	}
	add := func(merchant, total, currency, category string, at time.Time) {
		t.Helper()
		if _, err := service.Create(ctx, "主人", Draft{Merchant: merchant, SpentAt: &at, Currency: currency, Total: total, Category: category}); err != nil {
			t.Fatal(err)
		}
	}
	november := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	add("ロピア", "2300", "JPY", "groceries", november)
	add("ロピア", "1200", "JPY", "groceries", november.AddDate(0, 0, 1))
	add("猫砂店", "800", "JPY", pets.Key, november.AddDate(0, 0, 2))
	add("淘宝", "35.5", "CNY", "shopping", november.AddDate(0, 0, 3))
	add("ロピア", "999", "JPY", "groceries", november.AddDate(0, -1, 0))
	summary, err := service.Summarize(ctx, "主人", november, "JPY", money.SameCurrencyOnly{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Count != 4 || summary.Sum["JPY"] != 4300*money.MicrosPerUnit || summary.Sum["CNY"] != 35_500_000 ||
		summary.Previous["JPY"] != 999*money.MicrosPerUnit {
		t.Errorf("合计：%+v", summary)
	}
	if summary.Merchants[0].Key != "ロピア" || summary.Merchants[0].Count != 2 || summary.Categories[0].Key != "groceries" {
		t.Errorf("分组：%+v %+v", summary.Merchants, summary.Categories)
	}
	if list, _ := service.Expenses(ctx, "主人", Filter{From: november, To: november.AddDate(0, 1, 0), Category: pets.Key}); len(list) != 1 {
		t.Errorf("按分类列：%+v", list)
	}
	if err := service.DeleteCategory(ctx, "主人", pets.Key); err != nil {
		t.Fatal(err)
	}
}

// scriptedModel 交回一次 save_receipt。
type scriptedModel struct{ arguments map[string]any }

func (m scriptedModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		yield(brain.ReplyDelta{Stop: brain.StopCompleted, ToolCalls: []brain.ToolCall{{ID: "c", Name: saveTool, Arguments: m.arguments}}}, nil)
	}
}

// 代理用 save_receipt 交回：不是小票 → ErrUnreadable；分类不在清单里的不要；时间解不开的当成拿不准。
func TestAgentParser(t *testing.T) {
	categories := []Category{{Key: "groceries", Builtin: true}}
	parser := AgentParser{Agent: brain.Agent{Model: scriptedModel{arguments: map[string]any{"readable": false}}}}
	if _, err := parser.Parse(context.Background(), []byte("jpeg"), categories, Hint{}); !errors.Is(err, ErrUnreadable) {
		t.Errorf("不是小票：%v", err)
	}
	parser = AgentParser{Agent: brain.Agent{Model: scriptedModel{arguments: map[string]any{"readable": true, "merchant": "店", "currency": "jpy",
		"total": "100", "category": "yachts", "spent_at": "昨天", "items": []any{map[string]any{"name": "a", "amount": "100"}}}}}}
	draft, err := parser.Parse(context.Background(), []byte("jpeg"), categories, Hint{})
	if err != nil || draft.Category != "" || draft.Currency != "JPY" || draft.SpentAt != nil || len(draft.Uncertain) != 1 || len(draft.Items) != 1 {
		t.Errorf("草稿：%+v %v", draft, err)
	}
}

// blockingParser 一直解析到 release 关掉。
type blockingParser struct{ release chan struct{} }

func (p blockingParser) Parse(ctx context.Context, image []byte, categories []Category, hint Hint) (Draft, error) {
	select {
	case <-p.release:
	case <-ctx.Done():
		return Draft{}, ctx.Err()
	}
	return fakeParser{}.Parse(ctx, image, categories, hint)
}

// 一个人同时在解析的最多 MaxPending 张；解析中的不能丢掉、不能确认。
func TestPendingLimit(t *testing.T) {
	parser := blockingParser{release: make(chan struct{})}
	service, _ := newService(t, parser)
	ctx := context.Background()
	var first Receipt
	for i := range MaxPending {
		receipt, err := service.Upload(ctx, "主人", pngBytes(t), Hint{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = receipt
		}
	}
	if _, err := service.Upload(ctx, "主人", pngBytes(t), Hint{}); !errors.Is(err, ErrTooMany) {
		t.Errorf("第 %d 张：%v", MaxPending+1, err)
	}
	if _, err := service.Upload(ctx, "别人", pngBytes(t), Hint{}); err != nil {
		t.Errorf("别人的不受影响：%v", err)
	}
	if err := service.Discard(ctx, "主人", first.ID); !errors.Is(err, ErrState) {
		t.Errorf("解析中丢掉：%v", err)
	}
	spent := time.Now()
	if _, err := service.Confirm(ctx, "主人", first.ID, Draft{Merchant: "店", SpentAt: &spent, Currency: "JPY", Total: "1"}); !errors.Is(err, ErrState) {
		t.Errorf("解析中确认：%v", err)
	}
	close(parser.release)
	waitParsed(t, service, "主人", first.ID)
}

// 两个请求同时确认同一张：只入账一笔。
func TestConfirmOnce(t *testing.T) {
	service, _ := newService(t, fakeParser{})
	ctx := context.Background()
	receipt, err := service.Upload(ctx, "主人", pngBytes(t), Hint{})
	if err != nil {
		t.Fatal(err)
	}
	draft := *waitParsed(t, service, "主人", receipt.ID).Draft
	results := make(chan error, 4)
	for range cap(results) {
		go func() { _, err := service.Confirm(ctx, "主人", receipt.ID, draft); results <- err }()
	}
	succeeded := 0
	for range cap(results) {
		if err := <-results; err == nil {
			succeeded++
		} else if !errors.Is(err, ErrState) {
			t.Error(err)
		}
	}
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if list, _ := service.Expenses(ctx, "主人", Filter{From: month, To: month.AddDate(0, 1, 0)}); succeeded != 1 || len(list) != 1 {
		t.Errorf("成功 %d 次、入账 %d 笔", succeeded, len(list))
	}
}

// 过期的小票：没确认的连原图一起删；确认过的只删记录（原图跟着账目留着）。
func TestPrune(t *testing.T) {
	now := time.Now()
	store, err := OpenStore(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	files, err := attachments.OpenFiles(filepath.Join(t.TempDir(), "images"))
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return now }
	service := NewService(context.Background(), Options{Store: store, Images: files, Parser: fakeParser{}, Location: time.UTC, Logger: quiet,
		Now: func() time.Time { return clock() }})
	ctx := context.Background()
	abandoned, _ := service.Upload(ctx, "主人", pngBytes(t), Hint{})
	kept, _ := service.Upload(ctx, "主人", pngBytes(t), Hint{})
	waitParsed(t, service, "主人", abandoned.ID)
	expense, err := service.Confirm(ctx, "主人", kept.ID, *waitParsed(t, service, "主人", kept.ID).Draft)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(receiptRetention + time.Hour)
	clock = func() time.Time { return later }
	if err := service.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Receipt(ctx, "主人", abandoned.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("没确认的小票：%v", err)
	}
	if _, err := files.Read(abandoned.AttachmentID); err == nil {
		t.Error("没确认的小票的原图应删掉")
	}
	if _, err := service.Receipt(ctx, "主人", kept.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("确认过的小票记录：%v", err)
	}
	if _, err := service.ExpenseImage(ctx, "主人", expense.ID); err != nil {
		t.Errorf("账目的原图应留着：%v", err)
	}
}
