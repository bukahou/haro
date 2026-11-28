package appserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/expenses"
	"github.com/bukahou/haro/internal/money"
)

// Expenses 是记账（expenses.Service 实现）。
type Expenses interface {
	Upload(ctx context.Context, person string, data []byte, hint expenses.Hint) (expenses.Receipt, error)
	OpenReceipts(ctx context.Context, person string) ([]expenses.Receipt, error)
	Receipt(ctx context.Context, person, id string) (expenses.Receipt, error)
	ReceiptImage(ctx context.Context, person, id string) ([]byte, error)
	Confirm(ctx context.Context, person, id string, draft expenses.Draft) (expenses.Expense, error)
	Discard(ctx context.Context, person, id string) error
	Create(ctx context.Context, person string, draft expenses.Draft) (expenses.Expense, error)
	Expense(ctx context.Context, person, id string) (expenses.Expense, error)
	ExpenseImage(ctx context.Context, person, id string) ([]byte, error)
	Expenses(ctx context.Context, person string, filter expenses.Filter) ([]expenses.Expense, error)
	Update(ctx context.Context, person, id string, draft expenses.Draft) (expenses.Expense, error)
	Delete(ctx context.Context, person, id string) error
	Categories(ctx context.Context, person string) ([]expenses.Category, error)
	AddCategory(ctx context.Context, person, name string) (expenses.Category, error)
	DeleteCategory(ctx context.Context, person, key string) error
	ParseMonth(text string) (time.Time, error)
	MonthFilter(month time.Time) expenses.Filter
	Summarize(ctx context.Context, person string, month time.Time, currency string, converter money.Converter) (expenses.Summary, error)
}

// 小票的上传、两张原图的取回（二进制，手写挂载；上传的请求体上限和聊天的图片一样）。
const (
	receiptUploadPattern = "POST " + PathPrefix + "receipts"
	receiptImagePattern  = "GET " + PathPrefix + "receipts/{receiptId}/image"
	expenseImagePattern  = "GET " + PathPrefix + "expenses/{expenseId}/image"
)

// expensesProblem 是记账的错误对应的回答（按错误的类型分）；认不出的是 500。
func (s *Server) expensesProblem(ctx context.Context, what string, err error) problemReply {
	switch {
	case errors.Is(err, expenses.ErrNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil)
	case errors.Is(err, expenses.ErrState), errors.Is(err, expenses.ErrTooMany):
		return s.fail(ctx, http.StatusConflict, appapi.ErrorCodeConflict, nil)
	case errors.Is(err, expenses.ErrInvalid):
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": err.Error()})
	case errors.Is(err, attachments.ErrUnsupported):
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "image"})
	}
	return s.internalError(ctx, what, err)
}

// --- 转换

func draftOf(input appapi.ExpenseInput) expenses.Draft {
	spent := input.SpentAt
	draft := expenses.Draft{Merchant: input.Merchant, SpentAt: &spent, Currency: input.Currency, Total: input.Total,
		Subtotal: deref(input.Subtotal), Tax: deref(input.Tax), PaymentMethod: deref(input.PaymentMethod), Category: deref(input.Category),
		Note: deref(input.Note)}
	if input.Items != nil {
		for _, item := range *input.Items {
			draft.Items = append(draft.Items, expenses.DraftItem{Name: item.Name, Quantity: deref(item.Quantity), UnitPrice: deref(item.UnitPrice),
				Amount: item.Amount, TaxRate: deref(item.TaxRate)})
		}
	}
	return draft
}

func draftReply(draft expenses.Draft) appapi.ExpenseDraft {
	reply := appapi.ExpenseDraft{Merchant: draft.Merchant, SpentAt: draft.SpentAt, Currency: draft.Currency, Total: draft.Total,
		Subtotal: optional(draft.Subtotal), Tax: optional(draft.Tax), PaymentMethod: optional(draft.PaymentMethod), Category: optional(draft.Category),
		Note: optional(draft.Note), Items: []appapi.ExpenseItemInput{}, Uncertain: []appapi.ReceiptField{}}
	for _, item := range draft.Items {
		reply.Items = append(reply.Items, appapi.ExpenseItemInput{Name: item.Name, Quantity: optional(item.Quantity),
			UnitPrice: optional(item.UnitPrice), Amount: item.Amount, TaxRate: optional(item.TaxRate)})
	}
	for _, field := range draft.Uncertain {
		if known := appapi.ReceiptField(field); known.Valid() { // 只给契约里有的字段名（模型写错的不往外给）
			reply.Uncertain = append(reply.Uncertain, known)
		}
	}
	return reply
}

func (s *Server) receiptReply(ctx context.Context, receipt expenses.Receipt) appapi.Receipt {
	reply := appapi.Receipt{ReceiptId: receipt.ID, Status: appapi.ReceiptStatus(receipt.Status), CreatedAt: receipt.CreatedAt,
		ExpenseId: optional(receipt.ExpenseID)}
	if receipt.Draft != nil {
		draft := draftReply(*receipt.Draft)
		reply.Draft = &draft
	}
	if receipt.Failure != "" {
		failure := appapi.ReceiptFailure(receipt.Failure)
		message := s.texts.say(languageFrom(ctx), "receipt_failure."+receipt.Failure, nil)
		reply.Failure, reply.FailureMessage = &failure, &message
	}
	return reply
}

// categoryNamer 给分类键起给人看的名字：固定的按语言，自己加的照原样，认不出的（删了的）是「已删的分类」。
type categoryNamer struct {
	lang   string
	texts  *texts
	custom map[string]string
}

func (s *Server) categoryNamer(ctx context.Context, person string) (categoryNamer, error) {
	namer := categoryNamer{lang: languageFrom(ctx), texts: s.texts, custom: map[string]string{}}
	categories, err := s.opts.Expenses.Categories(ctx, person)
	if err != nil {
		return namer, err
	}
	for _, category := range categories {
		if !category.Builtin {
			namer.custom[category.Key] = category.Name
		}
	}
	return namer, nil
}

func (n categoryNamer) category(key string) appapi.ExpenseCategory {
	if name, ok := n.custom[key]; ok {
		return appapi.ExpenseCategory{Key: key, Name: name}
	}
	if isBuiltinCategory(key) {
		return appapi.ExpenseCategory{Key: key, Name: n.texts.say(n.lang, "expense_category."+key, nil), Builtin: true}
	}
	return appapi.ExpenseCategory{Key: key, Name: n.texts.say(n.lang, "expense_category.deleted", nil)}
}

func isBuiltinCategory(key string) bool {
	for _, builtin := range expenses.BuiltinCategories {
		if builtin == key {
			return true
		}
	}
	return false
}

func (n categoryNamer) expense(expense expenses.Expense) appapi.Expense {
	reply := appapi.Expense{ExpenseId: expense.ID, Merchant: expense.Merchant, SpentAt: expense.SpentAt, Total: moneyOf(expense.Total),
		PaymentMethod: optional(expense.PaymentMethod), Note: optional(expense.Note), Items: []appapi.ExpenseItem{},
		HasImage: expense.AttachmentID != "", CreatedAt: expense.CreatedAt, UpdatedAt: expense.UpdatedAt}
	if expense.Subtotal != nil {
		subtotal := moneyOf(*expense.Subtotal)
		reply.Subtotal = &subtotal
	}
	if expense.Tax != nil {
		tax := moneyOf(*expense.Tax)
		reply.Tax = &tax
	}
	if expense.Category != "" {
		category := n.category(expense.Category)
		reply.Category = &category
	}
	for _, item := range expense.Items {
		row := appapi.ExpenseItem{Name: item.Name, Amount: moneyOf(item.Amount), Quantity: optional(item.Quantity), TaxRate: optional(item.TaxRate)}
		if item.UnitPrice.Currency != "" {
			unit := moneyOf(item.UnitPrice)
			row.UnitPrice = &unit
		}
		reply.Items = append(reply.Items, row)
	}
	return reply
}

// expensesFor 是这次请求的账号和记账；没有记账时回 501。
func (s *Server) expensesFor(ctx context.Context) (string, *problemReply) {
	account, denied := s.me(ctx)
	if denied != nil {
		return "", denied
	}
	if s.opts.Expenses == nil {
		reply := s.notImplemented(ctx)
		return "", &reply
	}
	return account.OwnerPerson, nil
}

// --- 小票

// uploadReceipt 是交一张小票：请求体就是图片，分类、说明在查询参数里。
func (s *Server) uploadReceipt(w http.ResponseWriter, r *http.Request) {
	account, ok := s.bearerAccount(w, r)
	if !ok {
		return
	}
	if s.opts.Expenses == nil {
		s.writeProblem(w, r, http.StatusNotImplemented, appapi.ErrorCodeNotImplemented, nil)
		return
	}
	hint := expenses.Hint{Category: r.URL.Query().Get("category"), Note: r.URL.Query().Get("note")}
	if len(hint.Category) > 64 || utf8.RuneCountInString(hint.Note) > expenses.MaxNoteRunes { // 和契约 uploadReceipt 的参数一致
		s.writeProblem(w, r, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "query"})
		return
	}
	data, ok := s.readImage(w, r)
	if !ok {
		return
	}
	receipt, err := s.opts.Expenses.Upload(r.Context(), account.OwnerPerson, data, hint)
	if err != nil {
		reply := s.expensesProblem(r.Context(), "交小票", err)
		_ = reply.write(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(s.receiptReply(r.Context(), receipt))
}

// getReceiptImage 是小票的原图。
func (s *Server) getReceiptImage(w http.ResponseWriter, r *http.Request) {
	s.serveExpenseImage(w, r, func(ctx context.Context, person string) ([]byte, error) {
		return s.opts.Expenses.ReceiptImage(ctx, person, r.PathValue("receiptId"))
	})
}

// getExpenseImage 是一笔账的原图。
func (s *Server) getExpenseImage(w http.ResponseWriter, r *http.Request) {
	s.serveExpenseImage(w, r, func(ctx context.Context, person string) ([]byte, error) {
		return s.opts.Expenses.ExpenseImage(ctx, person, r.PathValue("expenseId"))
	})
}

func (s *Server) serveExpenseImage(w http.ResponseWriter, r *http.Request, image func(ctx context.Context, person string) ([]byte, error)) {
	account, ok := s.bearerAccount(w, r)
	if !ok {
		return
	}
	if s.opts.Expenses == nil {
		s.writeProblem(w, r, http.StatusNotImplemented, appapi.ErrorCodeNotImplemented, nil)
		return
	}
	data, err := image(r.Context(), account.OwnerPerson)
	if err != nil {
		reply := s.expensesProblem(r.Context(), "取小票的原图", err)
		_ = reply.write(w)
		return
	}
	writeImage(w, data)
}

// GetReceipt 实现 appapi.StrictServerInterface。
func (s *Server) GetReceipt(ctx context.Context, request appapi.GetReceiptRequestObject) (appapi.GetReceiptResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	receipt, err := s.opts.Expenses.Receipt(ctx, person, request.ReceiptId)
	if err != nil {
		return s.expensesProblem(ctx, "读小票", err), nil
	}
	return appapi.GetReceipt200JSONResponse(s.receiptReply(ctx, receipt)), nil
}

// ListReceipts 实现 appapi.StrictServerInterface。
func (s *Server) ListReceipts(ctx context.Context, _ appapi.ListReceiptsRequestObject) (appapi.ListReceiptsResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	receipts, err := s.opts.Expenses.OpenReceipts(ctx, person)
	if err != nil {
		return s.expensesProblem(ctx, "列小票", err), nil
	}
	reply := make(appapi.ListReceipts200JSONResponse, 0, len(receipts))
	for _, receipt := range receipts {
		reply = append(reply, s.receiptReply(ctx, receipt))
	}
	return reply, nil
}

// DiscardReceipt 实现 appapi.StrictServerInterface。
func (s *Server) DiscardReceipt(ctx context.Context, request appapi.DiscardReceiptRequestObject) (appapi.DiscardReceiptResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	if err := s.opts.Expenses.Discard(ctx, person, request.ReceiptId); err != nil {
		return s.expensesProblem(ctx, "删小票", err), nil
	}
	return appapi.DiscardReceipt204Response{}, nil
}

// ConfirmReceipt 实现 appapi.StrictServerInterface。
func (s *Server) ConfirmReceipt(ctx context.Context, request appapi.ConfirmReceiptRequestObject) (appapi.ConfirmReceiptResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	expense, err := s.opts.Expenses.Confirm(ctx, person, request.ReceiptId, draftOf(*request.Body))
	if err != nil {
		return s.expensesProblem(ctx, "确认小票", err), nil
	}
	reply, err := s.expenseReply(ctx, person, expense)
	if err != nil {
		return s.expensesProblem(ctx, "读分类", err), nil
	}
	return appapi.ConfirmReceipt201JSONResponse(reply), nil
}

// --- 账目

func (s *Server) expenseReply(ctx context.Context, person string, expense expenses.Expense) (appapi.Expense, error) {
	namer, err := s.categoryNamer(ctx, person)
	if err != nil {
		return appapi.Expense{}, err
	}
	return namer.expense(expense), nil
}

// ListExpenses 实现 appapi.StrictServerInterface。
func (s *Server) ListExpenses(ctx context.Context, request appapi.ListExpensesRequestObject) (appapi.ListExpensesResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	month, err := s.opts.Expenses.ParseMonth(deref(request.Params.Month))
	if err != nil {
		return s.expensesProblem(ctx, "读月份", err), nil
	}
	filter := s.opts.Expenses.MonthFilter(month)
	filter.Category, filter.Merchant = deref(request.Params.Category), deref(request.Params.Merchant)
	list, err := s.opts.Expenses.Expenses(ctx, person, filter)
	if err != nil {
		return s.expensesProblem(ctx, "列账目", err), nil
	}
	namer, err := s.categoryNamer(ctx, person)
	if err != nil {
		return s.expensesProblem(ctx, "读分类", err), nil
	}
	reply := appapi.ListExpenses200JSONResponse{}
	for _, expense := range list {
		reply = append(reply, namer.expense(expense))
	}
	return reply, nil
}

// CreateExpense 实现 appapi.StrictServerInterface。
func (s *Server) CreateExpense(ctx context.Context, request appapi.CreateExpenseRequestObject) (appapi.CreateExpenseResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	expense, err := s.opts.Expenses.Create(ctx, person, draftOf(*request.Body))
	if err != nil {
		return s.expensesProblem(ctx, "记账", err), nil
	}
	reply, err := s.expenseReply(ctx, person, expense)
	if err != nil {
		return s.expensesProblem(ctx, "读分类", err), nil
	}
	return appapi.CreateExpense201JSONResponse(reply), nil
}

// GetExpense 实现 appapi.StrictServerInterface。
func (s *Server) GetExpense(ctx context.Context, request appapi.GetExpenseRequestObject) (appapi.GetExpenseResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	expense, err := s.opts.Expenses.Expense(ctx, person, request.ExpenseId)
	if err != nil {
		return s.expensesProblem(ctx, "读账目", err), nil
	}
	reply, err := s.expenseReply(ctx, person, expense)
	if err != nil {
		return s.expensesProblem(ctx, "读分类", err), nil
	}
	return appapi.GetExpense200JSONResponse(reply), nil
}

// UpdateExpense 实现 appapi.StrictServerInterface。
func (s *Server) UpdateExpense(ctx context.Context, request appapi.UpdateExpenseRequestObject) (appapi.UpdateExpenseResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	expense, err := s.opts.Expenses.Update(ctx, person, request.ExpenseId, draftOf(*request.Body))
	if err != nil {
		return s.expensesProblem(ctx, "改账目", err), nil
	}
	reply, err := s.expenseReply(ctx, person, expense)
	if err != nil {
		return s.expensesProblem(ctx, "读分类", err), nil
	}
	return appapi.UpdateExpense200JSONResponse(reply), nil
}

// DeleteExpense 实现 appapi.StrictServerInterface。
func (s *Server) DeleteExpense(ctx context.Context, request appapi.DeleteExpenseRequestObject) (appapi.DeleteExpenseResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	if err := s.opts.Expenses.Delete(ctx, person, request.ExpenseId); err != nil {
		return s.expensesProblem(ctx, "删账目", err), nil
	}
	return appapi.DeleteExpense204Response{}, nil
}

// SummarizeExpenses 实现 appapi.StrictServerInterface。
func (s *Server) SummarizeExpenses(ctx context.Context, request appapi.SummarizeExpensesRequestObject) (appapi.SummarizeExpensesResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Expenses == nil {
		return s.notImplemented(ctx), nil
	}
	person := account.OwnerPerson
	month, err := s.opts.Expenses.ParseMonth(deref(request.Params.Month))
	if err != nil {
		return s.expensesProblem(ctx, "读月份", err), nil
	}
	currency, rates := s.currencyOf(account), s.rates()
	summary, err := s.opts.Expenses.Summarize(ctx, person, month, currency, rates)
	if err != nil {
		return s.expensesProblem(ctx, "汇总账目", err), nil
	}
	namer, err := s.categoryNamer(ctx, person)
	if err != nil {
		return s.expensesProblem(ctx, "读分类", err), nil
	}
	// total 换成默认货币的合计；micros 是换算后的数（算每组占这个月多少用）
	total := func(sum money.Sum) (appapi.MoneyTotal, int64, error) {
		converted, err := money.ConvertWhatCan(rates, sum, currency)
		return s.convertedTotal(ctx, sum, converted), converted.Amount.Micros, err
	}
	reply := appapi.ExpenseSummary{Month: summary.Month, Count: summary.Count,
		Categories: []appapi.ExpenseGroup{}, Merchants: []appapi.ExpenseGroup{}}
	var monthMicros int64
	if reply.Total, monthMicros, err = total(summary.Sum); err != nil {
		return s.expensesProblem(ctx, "换算账目", err), nil
	}
	if reply.Previous, _, err = total(summary.Previous); err != nil {
		return s.expensesProblem(ctx, "换算账目", err), nil
	}
	group := func(name string, key *string, sum money.Sum, count int) (appapi.ExpenseGroup, error) {
		item := appapi.ExpenseGroup{Count: count, Name: name, Key: key}
		var micros int64
		item.Total, micros, err = total(sum)
		if monthMicros > 0 {
			item.Share = min(float32(float64(micros)/float64(monthMicros)), 1)
		}
		return item, err
	}
	for _, category := range summary.Categories {
		name, key := s.texts.say(languageFrom(ctx), "expense_category.none", nil), (*string)(nil)
		if category.Key != "" {
			name, key = namer.category(category.Key).Name, &category.Key
		}
		item, err := group(name, key, category.Sum, category.Count)
		if err != nil {
			return s.expensesProblem(ctx, "换算账目", err), nil
		}
		reply.Categories = append(reply.Categories, item)
	}
	for _, merchant := range summary.Merchants {
		item, err := group(merchant.Key, nil, merchant.Sum, merchant.Count)
		if err != nil {
			return s.expensesProblem(ctx, "换算账目", err), nil
		}
		reply.Merchants = append(reply.Merchants, item)
	}
	return appapi.SummarizeExpenses200JSONResponse(reply), nil
}

// rates 是换算用的汇率；没配时只认同一种币种。
func (s *Server) rates() money.Converter {
	return cmp.Or[money.Converter](s.opts.Rates, money.SameCurrencyOnly{})
}

// --- 分类

// ListExpenseCategories 实现 appapi.StrictServerInterface。
func (s *Server) ListExpenseCategories(ctx context.Context, _ appapi.ListExpenseCategoriesRequestObject) (appapi.ListExpenseCategoriesResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	namer, err := s.categoryNamer(ctx, person)
	if err != nil {
		return s.expensesProblem(ctx, "读分类", err), nil
	}
	categories, err := s.opts.Expenses.Categories(ctx, person)
	if err != nil {
		return s.expensesProblem(ctx, "读分类", err), nil
	}
	reply := appapi.ListExpenseCategories200JSONResponse{}
	for _, category := range categories {
		reply = append(reply, namer.category(category.Key))
	}
	return reply, nil
}

// AddExpenseCategory 实现 appapi.StrictServerInterface。
func (s *Server) AddExpenseCategory(ctx context.Context, request appapi.AddExpenseCategoryRequestObject) (appapi.AddExpenseCategoryResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	category, err := s.opts.Expenses.AddCategory(ctx, person, request.Body.Name)
	if err != nil {
		return s.expensesProblem(ctx, "加分类", err), nil
	}
	return appapi.AddExpenseCategory201JSONResponse{Key: category.Key, Name: category.Name}, nil
}

// DeleteExpenseCategory 实现 appapi.StrictServerInterface。
func (s *Server) DeleteExpenseCategory(ctx context.Context, request appapi.DeleteExpenseCategoryRequestObject) (appapi.DeleteExpenseCategoryResponseObject, error) {
	person, denied := s.expensesFor(ctx)
	if denied != nil {
		return *denied, nil
	}
	if err := s.opts.Expenses.DeleteCategory(ctx, person, request.CategoryKey); err != nil {
		return s.expensesProblem(ctx, "删分类", err), nil
	}
	return appapi.DeleteExpenseCategory204Response{}, nil
}
