package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/expenses"
)

// scriptedParser 交回测试指定的结果（unreadable 为 true 时报看不清）。
type scriptedParser struct{ unreadable atomic.Bool }

func (p *scriptedParser) Parse(_ context.Context, _ []byte, _ []expenses.Category, hint expenses.Hint) (expenses.Draft, error) {
	if p.unreadable.Load() {
		return expenses.Draft{}, expenses.ErrUnreadable
	}
	spent := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	return expenses.Draft{Merchant: "ロピア", SpentAt: &spent, Currency: "JPY", Total: "2300", Category: hint.Category,
		Items: []expenses.DraftItem{{Name: "白菜", Amount: "198"}}, Uncertain: []string{"payment_method", "不是字段名"}}, nil
}

func newExpensesFixture(t *testing.T) (fixture, *scriptedParser) {
	t.Helper()
	ctx := t.Context()
	store, err := accounts.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	admin, _ := store.EnsureAdmin(ctx, accounts.Admin{Subject: "sub-owner", DisplayName: "主人", Person: "主人", GuestPerson: brain.SharedPerson})
	pair, _ := store.IssueTokens(ctx, admin.ID)
	books, err := expenses.OpenStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = books.Close() })
	images, err := attachments.OpenFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parser := &scriptedParser{}
	service := expenses.NewService(ctx, expenses.Options{Store: books, Images: images, Parser: parser, Location: time.UTC, Logger: quietLogger})
	handler, err := NewHandler(ctx, Options{Accounts: store, Spending: fakeSpending{}, DefaultCurrency: "JPY", MinAppVersion: "1.0.0",
		ProfileMaxChars: 10000, Expenses: service, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return fixture{server: server, store: store, admin: admin, adminKey: pair}, parser
}

// raw 发一个请求体是图片（或什么都没有）的请求，返回状态、Content-Type 和响应体。
func (f fixture) raw(t *testing.T, method, path, contentType string, data []byte) (int, string, []byte) {
	t.Helper()
	request, _ := http.NewRequest(method, f.server.URL+path, bytes.NewReader(data))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Authorization", "Bearer "+f.adminKey.AccessToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(response.Body)
	return response.StatusCode, response.Header.Get("Content-Type"), body.Bytes()
}

func (f fixture) waitReceipt(t *testing.T, id string) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, receipt := f.call(t, "GET", "/api/v1/receipts/"+id, f.adminKey.AccessToken, ""); receipt["status"] != "parsing" {
			return receipt
		}
	}
	t.Fatal("一直在解析")
	return nil
}

// 记账：分类（固定的按语言、自己加的）→ 交小票（202 在解析）→ 草稿（不是字段名的不往外给）→ 原图 → 确认入账（分类带名字、有原图）
// → 再确认 409 → 手记一笔别的币种 → 删自己的分类后显示「已删的分类」→ 按月列、按分类列 → 汇总（没有汇率的币种单独注明）
// → 删账目以后原图 404；看不清的小票：失败原因 + 说明，可以丢掉。
func TestExpensesOverHTTP(t *testing.T) {
	f, parser := newExpensesFixture(t)
	key := f.adminKey.AccessToken
	categories := f.list(t, "/api/v1/expense-categories", "zh")
	if len(categories) != len(expenses.BuiltinCategories) || categories[0]["name"] != "食品杂货" || categories[0]["builtin"] != true {
		t.Fatalf("分类：%v", categories)
	}
	status, pets := f.call(t, "POST", "/api/v1/expense-categories", key, `{"name":"猫"}`)
	if status != 201 || pets["name"] != "猫" || pets["builtin"] != false {
		t.Fatalf("加分类：%d %v", status, pets)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 30, 60))); err != nil {
		t.Fatal(err)
	}
	status, _, body := f.raw(t, "POST", "/api/v1/receipts?category=groceries&note=%E4%B9%B0%E8%8F%9C", "image/png", encoded.Bytes())
	var uploaded map[string]any
	_ = json.Unmarshal(body, &uploaded)
	if status != 202 || uploaded["status"] != "parsing" {
		t.Fatalf("交小票：%d %s", status, body)
	}
	receiptID := uploaded["receipt_id"].(string)
	receipt := f.waitReceipt(t, receiptID)
	draft, _ := receipt["draft"].(map[string]any)
	if receipt["status"] != "ready" || draft["merchant"] != "ロピア" || draft["category"] != "groceries" || len(draft["uncertain"].([]any)) != 1 {
		t.Fatalf("草稿：%v", receipt)
	}
	if status, media, data := f.raw(t, "GET", "/api/v1/receipts/"+receiptID+"/image", "", nil); status != 200 || media != "image/jpeg" {
		t.Errorf("小票的原图：%d %s", status, media)
	} else if _, err := jpeg.DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Errorf("原图不是 JPEG：%v", err)
	}
	confirm := `{"merchant":"ロピア","spent_at":"2026-11-10T03:00:00Z","currency":"JPY","total":"2310","payment_method":"PayPay",
		"category":"groceries","items":[{"name":"白菜","amount":"198"},{"name":"豚肉","quantity":"2","unit_price":"551","amount":"1102","tax_rate":"8"}]}`
	status, expense := f.call(t, "POST", "/api/v1/receipts/"+receiptID+"/confirm", key, confirm)
	if status != 201 || expense["has_image"] != true || expense["category"].(map[string]any)["name"] != "食品杂货" ||
		expense["total"].(map[string]any)["amount"] != "2310" || expense["items"].([]any)[1].(map[string]any)["unit_price"].(map[string]any)["amount"] != "551" {
		t.Fatalf("入账：%d %v", status, expense)
	}
	expenseID := expense["expense_id"].(string)
	if status, _ := f.call(t, "POST", "/api/v1/receipts/"+receiptID+"/confirm", key, confirm); status != 409 {
		t.Errorf("确认两次：%d", status)
	}
	manual := `{"merchant":"淘宝","spent_at":"2026-11-12T03:00:00Z","currency":"CNY","total":"35.5","category":"` + pets["key"].(string) + `"}`
	status, other := f.call(t, "POST", "/api/v1/expenses", key, manual)
	if status != 201 || other["has_image"] != false {
		t.Fatalf("手记：%d %v", status, other)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/expense-categories/"+pets["key"].(string), key, ""); status != 204 {
		t.Errorf("删分类：%d", status)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/expense-categories/groceries", key, ""); status != 404 {
		t.Errorf("固定的分类删不了：%d", status)
	}
	if _, again := f.call(t, "GET", "/api/v1/expenses/"+other["expense_id"].(string), key, ""); again["category"].(map[string]any)["name"] != "已删的分类" {
		t.Errorf("删了的分类：%v", again)
	}
	if list := f.list(t, "/api/v1/expenses?month=2026-11", "zh"); len(list) != 2 || list[0]["merchant"] != "淘宝" {
		t.Errorf("按月列：%v", list)
	}
	if list := f.list(t, "/api/v1/expenses?month=2026-11&category=groceries", "zh"); len(list) != 1 {
		t.Errorf("按分类列：%v", list)
	}
	if status, _ := f.call(t, "GET", "/api/v1/expenses?month=2026-13", key, ""); status != 400 {
		t.Errorf("月份不对：%d", status)
	}
	status, summary := f.call(t, "GET", "/api/v1/expenses/summary?month=2026-11", key, "")
	total, _ := summary["total"].(map[string]any)
	if status != 200 || summary["count"] != float64(2) || total["total"].(map[string]any)["amount"] != "2310" ||
		len(total["by_currency"].([]any)) != 2 || total["conversion_note"] == nil {
		t.Fatalf("汇总：%d %v", status, summary)
	}
	if groups := summary["categories"].([]any); len(groups) != 2 || groups[0].(map[string]any)["name"] != "食品杂货" {
		t.Errorf("按分类：%v", groups)
	}
	if status, body := f.call(t, "POST", "/api/v1/expenses", key, `{"merchant":"店","spent_at":"2026-11-12T03:00:00Z","currency":"JPY","total":"一百"}`); status != 400 ||
		body["code"] != "validation_failed" {
		t.Errorf("合计不是数：%d %v", status, body)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/expenses/"+expenseID, key, ""); status != 204 {
		t.Errorf("删账目：%d", status)
	}
	if status, _, _ := f.raw(t, "GET", "/api/v1/expenses/"+expenseID+"/image", "", nil); status != 404 {
		t.Errorf("删了以后的原图：%d", status)
	}

	parser.unreadable.Store(true)
	_, _, body = f.raw(t, "POST", "/api/v1/receipts", "image/png", encoded.Bytes())
	_ = json.Unmarshal(body, &uploaded)
	failed := f.waitReceipt(t, uploaded["receipt_id"].(string))
	if failed["status"] != "failed" || failed["failure"] != "unreadable" || failed["failure_message"] == nil {
		t.Errorf("看不清：%v", failed)
	}
	if status, _ := f.call(t, "DELETE", "/api/v1/receipts/"+uploaded["receipt_id"].(string), key, ""); status != 204 {
		t.Errorf("丢掉：%d", status)
	}
	if status, _, _ := f.raw(t, "POST", "/api/v1/receipts", "text/plain", []byte("x")); status != 400 {
		t.Errorf("格式不对：%d", status)
	}

	contract, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for media := range contract.Paths.Find("/api/v1/receipts").Post.RequestBody.Value.Content {
		types = append(types, media)
	}
	slices.Sort(types)
	if want := slices.Sorted(slices.Values(uploadedTypes)); !slices.Equal(types, want) {
		t.Errorf("契约里小票收的图片格式 %v，代码里 %v", types, want)
	}
}

// 每个固定的分类、每种失败原因都有文字（名字只在 texts.json 里；loadTexts 守着每种语言都有默认语言的每一项）。
func TestBuiltinCategoryNames(t *testing.T) {
	loaded, err := loadTexts()
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"expense_category.deleted", "expense_category.none", "conversion_missing"}
	for _, key := range expenses.BuiltinCategories {
		keys = append(keys, "expense_category."+key)
	}
	for _, failure := range []string{expenses.FailureUnreadable, expenses.FailureUnavailable} {
		keys = append(keys, "receipt_failure."+failure)
	}
	for _, key := range keys {
		if loaded.Languages[loaded.Default][key] == "" {
			t.Errorf("没有 %s 的文字", key)
		}
	}
}
