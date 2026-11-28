package expenses

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/money"
)

// Images 存、取、删原图（attachments.Files 实现）。
type Images interface {
	Write(id string, data []byte) error
	Read(id string) ([]byte, error)
	Remove(id string) error
}

// Service 是记账。
type Service struct {
	lifetime context.Context // 后台解析跟着它停
	store    *Store
	images   Images
	parser   Parser
	location *time.Location // 按月汇总的「月」按哪个时区
	now      func() time.Time
	logger   *slog.Logger
	slots    chan struct{} // 同时解析几张
}

// Options 是记账的部件。
type Options struct {
	Store    *Store
	Images   Images
	Parser   Parser
	Location *time.Location // nil = time.Local
	Now      func() time.Time
	Logger   *slog.Logger
}

// NewService 建记账；后台解析随 lifetime 停。
func NewService(lifetime context.Context, opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{lifetime: lifetime, store: opts.Store, images: opts.Images, parser: opts.Parser, location: cmp.Or(opts.Location, time.Local),
		now: now, logger: opts.Logger.With("component", "expenses"), slots: make(chan struct{}, parseConcurrency)}
}

// Run 先把服务端重启时还没解析完的小票接着解析，之后每天清一次过期的小票；ctx 结束时返回。
func (s *Service) Run(ctx context.Context) {
	pending, err := s.store.parsingReceipts(ctx)
	if err != nil {
		s.logger.Error("读没解析完的小票出错", "err", err)
	}
	for _, receipt := range pending {
		go s.parse(receipt)
	}
	if len(pending) > 0 {
		s.logger.Info("接着解析服务端重启前没解析完的小票", "receipts", len(pending))
	}
	ticker := time.NewTicker(pruneEvery)
	defer ticker.Stop()
	for {
		if err := s.Prune(ctx); err != nil {
			s.logger.Error("清过期的小票出错", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Prune 清过期的小票：交上来 ReceiptRetention 以后还没确认的连原图一起删；确认过的只删记录（原图跟着账目）。
func (s *Service) Prune(ctx context.Context) error {
	stale, err := s.store.staleReceipts(ctx, s.now().Add(-ReceiptRetention))
	if err != nil {
		return err
	}
	for _, receipt := range stale {
		if err := s.store.dropReceipt(ctx, receipt.ID); err != nil {
			return err
		}
		if receipt.Status != StatusConfirmed {
			if err := s.images.Remove(receipt.AttachmentID); err != nil {
				s.logger.Warn("删过期小票的原图出错（记录已删）", "receipt", receipt.ID, "err", err)
			}
		}
	}
	if len(stale) > 0 {
		s.logger.Info("清掉过期的小票", "receipts", len(stale))
	}
	return nil
}

// Upload 交一张小票（图片：JPEG / PNG / WebP）：存成 JPEG、开始在后台解析，返回小票（状态 parsing）。
func (s *Service) Upload(ctx context.Context, person string, data []byte, hint Hint) (Receipt, error) {
	if hint.Category != "" {
		if ok, err := s.knownCategory(ctx, person, hint.Category); err != nil || !ok {
			return Receipt{}, errors.Join(fmt.Errorf("%w：没有这个分类", ErrInvalid), err)
		}
	}
	if utf8.RuneCountInString(hint.Note) > MaxNoteRunes {
		return Receipt{}, fmt.Errorf("%w：说明最多 %d 字", ErrInvalid, MaxNoteRunes)
	}
	if pending, err := s.store.pendingCount(ctx, person); err != nil {
		return Receipt{}, err
	} else if pending >= MaxPending {
		return Receipt{}, ErrTooMany
	}
	encoded, _, _, err := attachments.Normalize(data)
	if err != nil {
		return Receipt{}, err
	}
	id, err := randomID()
	if err != nil {
		return Receipt{}, err
	}
	attachment, err := randomID()
	if err != nil {
		return Receipt{}, err
	}
	if err := s.images.Write(attachment, encoded); err != nil {
		return Receipt{}, err
	}
	now := s.now().UTC().Truncate(time.Second)
	receipt := Receipt{ID: id, Person: person, AttachmentID: attachment, CategoryHint: hint.Category, NoteHint: strings.TrimSpace(hint.Note),
		Status: StatusParsing, CreatedAt: now, UpdatedAt: now}
	if err := s.store.insertReceipt(ctx, receipt); err != nil {
		return Receipt{}, errors.Join(err, s.images.Remove(attachment))
	}
	go s.parse(receipt)
	return receipt, nil
}

// parse 在后台解析一张小票（同时最多 parseConcurrency 张），结果存回小票。
func (s *Service) parse(receipt Receipt) {
	select {
	case s.slots <- struct{}{}:
	case <-s.lifetime.Done():
		return
	}
	defer func() { <-s.slots }()
	ctx, cancel := context.WithTimeout(s.lifetime, parseTimeout)
	defer cancel()
	image, err := s.images.Read(receipt.AttachmentID)
	if err != nil {
		s.finishParse(receipt, nil, FailureUnreadable, err)
		return
	}
	categories, err := s.Categories(ctx, receipt.Person)
	if err != nil {
		s.finishParse(receipt, nil, FailureUnavailable, err)
		return
	}
	draft, err := s.parser.Parse(brain.WithBillingPerson(ctx, receipt.Person), image, categories, Hint{Category: receipt.CategoryHint, Note: receipt.NoteHint})
	switch {
	case s.lifetime.Err() != nil: // 服务端在停：留着 parsing，重启后接着解析
		return
	case errors.Is(err, ErrUnreadable):
		s.finishParse(receipt, nil, FailureUnreadable, nil)
	case err != nil:
		s.finishParse(receipt, nil, FailureUnavailable, err)
	default:
		s.finishParse(receipt, &draft, "", nil)
	}
}

func (s *Service) finishParse(receipt Receipt, draft *Draft, failure string, err error) {
	receipt.Status, receipt.Draft, receipt.Failure, receipt.UpdatedAt = StatusReady, draft, failure, s.now().UTC().Truncate(time.Second)
	if failure != "" {
		receipt.Status = StatusFailed
		s.logger.Warn("小票没解析成", "receipt", receipt.ID, "failure", failure, "err", err)
	} else {
		s.logger.Info("小票解析好了", "receipt", receipt.ID, "items", len(draft.Items), "uncertain", draft.Uncertain)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.lifetime), 10*time.Second)
	defer cancel()
	if err := s.store.finishParse(ctx, receipt); err != nil {
		s.logger.Error("存小票的解析结果出错", "receipt", receipt.ID, "err", err)
	}
}

// OpenReceipts 是这个人还没入账的小票：在解析的、解析好了等确认的、没解析成的（新的在前）。
func (s *Service) OpenReceipts(ctx context.Context, person string) ([]Receipt, error) {
	return s.store.openReceipts(ctx, person)
}

// Receipt 是这个人的一张小票。
func (s *Service) Receipt(ctx context.Context, person, id string) (Receipt, error) {
	receipt, err := s.store.receipt(ctx, id)
	if err != nil || receipt.Person != person {
		return Receipt{}, cmp.Or(err, ErrNotFound)
	}
	return receipt, nil
}

// ReceiptImage 是这个人一张小票的原图（JPEG）。
func (s *Service) ReceiptImage(ctx context.Context, person, id string) ([]byte, error) {
	receipt, err := s.Receipt(ctx, person, id)
	if err != nil {
		return nil, err
	}
	return s.images.Read(receipt.AttachmentID)
}

// Confirm 交回（可能改过的）草稿：校验后入账，小票标成已入账。解析中、解析失败、已经入账的不行（ErrState）。
func (s *Service) Confirm(ctx context.Context, person, id string, draft Draft) (Expense, error) {
	receipt, err := s.Receipt(ctx, person, id)
	if err != nil {
		return Expense{}, err
	}
	if receipt.Status != StatusReady {
		return Expense{}, ErrState
	}
	expense, err := s.expenseOf(ctx, person, draft)
	if err != nil {
		return Expense{}, err
	}
	expense.AttachmentID = receipt.AttachmentID
	if err := s.store.confirm(ctx, receipt.ID, draft, expense); err != nil {
		return Expense{}, err
	}
	return expense, nil
}

// Discard 不要这张小票了（解析好了不想入账、或者解析失败了要重拍）：连原图一起删。还在解析、已经入账的不行（ErrState）。
func (s *Service) Discard(ctx context.Context, person, id string) error {
	receipt, err := s.Receipt(ctx, person, id)
	if err != nil {
		return err
	}
	if err := s.store.deleteReceipt(ctx, id); err != nil {
		return err
	}
	if err := s.images.Remove(receipt.AttachmentID); err != nil {
		s.logger.Warn("删小票的原图出错（小票已删）", "receipt", id, "err", err)
	}
	return nil
}

// Create 手记一笔（没有小票）。
func (s *Service) Create(ctx context.Context, person string, draft Draft) (Expense, error) {
	expense, err := s.expenseOf(ctx, person, draft)
	if err != nil {
		return Expense{}, err
	}
	return expense, s.store.putExpense(ctx, expense)
}

// expenseOf 校验草稿、换成账目（金额精确解析；币种按 ISO 4217）。
func (s *Service) expenseOf(ctx context.Context, person string, draft Draft) (Expense, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w：%s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	merchant := strings.TrimSpace(draft.Merchant)
	switch {
	case merchant == "":
		return Expense{}, invalid("要有店名")
	case utf8.RuneCountInString(merchant) > MaxTextRunes || utf8.RuneCountInString(draft.PaymentMethod) > MaxTextRunes:
		return Expense{}, invalid("店名、付款方式最多 %d 字", MaxTextRunes)
	case utf8.RuneCountInString(draft.Note) > MaxNoteRunes:
		return Expense{}, invalid("说明最多 %d 字", MaxNoteRunes)
	case draft.SpentAt == nil:
		return Expense{}, invalid("要有消费的时间")
	case len(draft.Items) > MaxItems:
		return Expense{}, invalid("明细最多 %d 行", MaxItems)
	}
	total, err := money.ParseAmount(draft.Total, draft.Currency)
	if err != nil {
		return Expense{}, invalid("合计：%v", err)
	}
	if draft.Category != "" {
		if ok, err := s.knownCategory(ctx, person, draft.Category); err != nil {
			return Expense{}, err
		} else if !ok {
			return Expense{}, invalid("没有这个分类")
		}
	}
	now := s.now().UTC().Truncate(time.Second)
	id, err := randomID()
	if err != nil {
		return Expense{}, err
	}
	expense := Expense{ID: id, Person: person, Merchant: merchant, SpentAt: draft.SpentAt.UTC().Truncate(time.Second), Total: total,
		PaymentMethod: strings.TrimSpace(draft.PaymentMethod), Category: draft.Category, Note: strings.TrimSpace(draft.Note), CreatedAt: now, UpdatedAt: now}
	for field, text := range map[string]string{"subtotal": draft.Subtotal, "tax": draft.Tax} {
		if strings.TrimSpace(text) == "" {
			continue
		}
		amount, err := money.ParseAmount(text, total.Currency)
		if err != nil {
			return Expense{}, invalid("%s：%v", field, err)
		}
		if field == "subtotal" {
			expense.Subtotal = &amount
		} else {
			expense.Tax = &amount
		}
	}
	for i, row := range draft.Items {
		name := strings.TrimSpace(row.Name)
		if name == "" || utf8.RuneCountInString(name) > MaxTextRunes {
			return Expense{}, invalid("第 %d 行明细：要有名称、最多 %d 字", i+1, MaxTextRunes)
		}
		amount, err := money.ParseAmount(row.Amount, total.Currency)
		if err != nil {
			return Expense{}, invalid("第 %d 行明细的金额：%v", i+1, err)
		}
		item := Item{Name: name, Amount: amount, Quantity: strings.TrimSpace(row.Quantity), TaxRate: strings.TrimSpace(row.TaxRate)}
		if strings.TrimSpace(row.UnitPrice) != "" {
			if item.UnitPrice, err = money.ParseAmount(row.UnitPrice, total.Currency); err != nil {
				return Expense{}, invalid("第 %d 行明细的单价：%v", i+1, err)
			}
		}
		for _, number := range []string{item.Quantity, item.TaxRate} {
			if number != "" {
				if _, err := money.ParseAmount(number, total.Currency); err != nil {
					return Expense{}, invalid("第 %d 行明细的数量 / 税率不是数字", i+1)
				}
			}
		}
		expense.Items = append(expense.Items, item)
	}
	return expense, nil
}

// Expense 是这个人的一笔账。
func (s *Service) Expense(ctx context.Context, person, id string) (Expense, error) {
	expense, err := s.store.expense(ctx, id)
	if err != nil || expense.Person != person {
		return Expense{}, cmp.Or(err, ErrNotFound)
	}
	return expense, nil
}

// ExpenseImage 是一笔账的原图（手记的没有：ErrNotFound）。
func (s *Service) ExpenseImage(ctx context.Context, person, id string) ([]byte, error) {
	expense, err := s.Expense(ctx, person, id)
	if err != nil {
		return nil, err
	}
	if expense.AttachmentID == "" {
		return nil, ErrNotFound
	}
	return s.images.Read(expense.AttachmentID)
}

// Expenses 是这个人符合条件的账目（新的在前）。
func (s *Service) Expenses(ctx context.Context, person string, filter Filter) ([]Expense, error) {
	return s.store.list(ctx, person, filter)
}

// Update 整笔替换（原图、建的时间不变）。
func (s *Service) Update(ctx context.Context, person, id string, draft Draft) (Expense, error) {
	original, err := s.Expense(ctx, person, id)
	if err != nil {
		return Expense{}, err
	}
	updated, err := s.expenseOf(ctx, person, draft)
	if err != nil {
		return Expense{}, err
	}
	updated.ID, updated.AttachmentID, updated.CreatedAt = original.ID, original.AttachmentID, original.CreatedAt
	return updated, s.store.putExpense(ctx, updated)
}

// Delete 删一笔账，原图跟着删。
func (s *Service) Delete(ctx context.Context, person, id string) error {
	expense, err := s.Expense(ctx, person, id)
	if err != nil {
		return err
	}
	if err := s.store.deleteExpense(ctx, id); err != nil {
		return err
	}
	if expense.AttachmentID != "" {
		if err := s.images.Remove(expense.AttachmentID); err != nil {
			s.logger.Warn("删原图出错（账目已删）", "expense", id, "err", err)
		}
	}
	return nil
}

// Categories 是这个人能选的分类：固定的几类在前，主人加的在后。
func (s *Service) Categories(ctx context.Context, person string) ([]Category, error) {
	custom, err := s.store.customCategories(ctx, person)
	if err != nil {
		return nil, err
	}
	categories := make([]Category, 0, len(BuiltinCategories)+len(custom))
	for _, key := range BuiltinCategories {
		categories = append(categories, Category{Key: key, Builtin: true})
	}
	return append(categories, custom...), nil
}

func (s *Service) knownCategory(ctx context.Context, person, key string) (bool, error) {
	categories, err := s.Categories(ctx, person)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(categories, func(category Category) bool { return category.Key == key }), nil
}

// AddCategory 加一个主人自己的分类（键随机，名字照原样）。
func (s *Service) AddCategory(ctx context.Context, person, name string) (Category, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxTextRunes {
		return Category{}, fmt.Errorf("%w：分类的名字不能为空、最多 %d 字", ErrInvalid, MaxTextRunes)
	}
	id, err := randomID()
	if err != nil {
		return Category{}, err
	}
	category := Category{Key: "custom-" + id[:12], Name: name}
	return category, s.store.addCategory(ctx, person, category.Key, name, s.now())
}

// DeleteCategory 删一个主人自己的分类（用过它的账目的分类键留着；接口把认不出的键显示成「已删的分类」）。
func (s *Service) DeleteCategory(ctx context.Context, person, key string) error {
	return s.store.deleteCategory(ctx, person, key)
}

// monthRange 是 month 所在的那个月（按服务端的时区）。
func (s *Service) monthRange(month time.Time) (from, to time.Time) {
	local := month.In(s.location)
	from = time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, s.location)
	return from.UTC(), from.AddDate(0, 1, 0).UTC()
}

// MonthLayout 是月份的写法（YYYY-MM）。
const MonthLayout = "2006-01"

// ParseMonth 把「YYYY-MM」变成那个月（按记账的时区）；空 = 这个月。
func (s *Service) ParseMonth(text string) (time.Time, error) {
	if text == "" {
		return s.now().In(s.location), nil
	}
	month, err := time.ParseInLocation(MonthLayout, text, s.location)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w：月份要写成 YYYY-MM", ErrInvalid)
	}
	return month, nil
}

// MonthFilter 是 month 那个月的条件（再按需要加分类、店名）。
func (s *Service) MonthFilter(month time.Time) Filter {
	from, to := s.monthRange(month)
	return Filter{From: from, To: to}
}

// Group 是汇总里的一组（一个分类、一家店）：按币种分开的合计和笔数。
type Group struct {
	Key   string
	Sum   money.Sum
	Count int
}

// Summary 是一个月的汇总：按币种分开的合计、按分类、按店（各自按合计排）；上个月的合计（比较用）。
type Summary struct {
	Month      string // YYYY-MM
	From, To   time.Time
	Count      int
	Sum        money.Sum
	Categories []Group
	Merchants  []Group
	Previous   money.Sum
}

// Summarize 汇总 month 那个月；converter 把按币种分开的合计换成默认货币（排序按换算后的大小）。
func (s *Service) Summarize(ctx context.Context, person string, month time.Time, currency string, converter money.Converter) (Summary, error) {
	from, to := s.monthRange(month)
	current, err := s.store.list(ctx, person, Filter{From: from, To: to})
	if err != nil {
		return Summary{}, err
	}
	previousFrom, _ := s.monthRange(from.In(s.location).AddDate(0, -1, 0))
	previous, err := s.store.list(ctx, person, Filter{From: previousFrom, To: from})
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{Month: from.In(s.location).Format(MonthLayout), From: from, To: to, Count: len(current), Sum: money.Sum{}, Previous: money.Sum{}}
	categories, merchants := map[string]*Group{}, map[string]*Group{}
	for _, expense := range current {
		summary.Sum.Add(expense.Total)
		for _, grouped := range []struct {
			groups map[string]*Group
			key    string
		}{{categories, expense.Category}, {merchants, expense.Merchant}} {
			group, ok := grouped.groups[grouped.key]
			if !ok {
				group = &Group{Key: grouped.key, Sum: money.Sum{}}
				grouped.groups[grouped.key] = group
			}
			group.Sum.Add(expense.Total)
			group.Count++
		}
	}
	for _, expense := range previous {
		summary.Previous.Add(expense.Total)
	}
	size := func(sum money.Sum) int64 {
		total, err := converter.Convert(sum, currency)
		if err != nil {
			return 0
		}
		return total.Amount.Micros
	}
	sorted := func(groups map[string]*Group) []Group {
		out := make([]Group, 0, len(groups))
		for _, group := range groups {
			out = append(out, *group)
		}
		slices.SortFunc(out, func(a, b Group) int { return cmp.Or(cmp.Compare(size(b.Sum), size(a.Sum)), cmp.Compare(a.Key, b.Key)) })
		return out
	}
	summary.Categories, summary.Merchants = sorted(categories), sorted(merchants)
	return summary, nil
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ErasePerson 删掉这个人的全部账目、小票（连原图）、自己加的分类（彻底删账号时用）。
func (s *Service) ErasePerson(ctx context.Context, person string) error {
	images, err := s.store.erasePerson(ctx, person)
	if err != nil {
		return err
	}
	for _, id := range images {
		if err := s.images.Remove(id); err != nil {
			s.logger.Warn("删原图出错（记录已删）", "image", id, "err", err)
		}
	}
	return nil
}
