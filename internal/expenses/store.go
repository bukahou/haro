package expenses

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bukahou/haro/internal/money"
	"github.com/bukahou/haro/internal/sqliteschema"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store 把账目、小票、主人加的分类存在一个 SQLite 文件里。
type Store struct{ db *sql.DB }

// OpenStore 打开（没有就建）账目的库并升到最新。库里是主人的消费记录：目录 0700、文件 0600。path 为空 = 只在内存里。
func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sqliteschema.Open(ctx, path, migrationFiles, sqliteschema.Options{Private: true})
	if err != nil {
		return nil, fmt.Errorf("打开账目的库 %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close 关掉库。
func (s *Store) Close() error { return s.db.Close() }

// --- 小票

const receiptColumns = `id, person, attachment_id, category_hint, note_hint, status, draft, failure, expense_id, created_at, updated_at`

func (s *Store) insertReceipt(ctx context.Context, receipt Receipt) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO receipts (`+receiptColumns+`) VALUES (?, ?, ?, ?, ?, ?, NULL, '', NULL, ?, ?)`,
		receipt.ID, receipt.Person, receipt.AttachmentID, receipt.CategoryHint, receipt.NoteHint, receipt.Status,
		receipt.CreatedAt.Unix(), receipt.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("存小票: %w", err)
	}
	return nil
}

// finishParse 存解析的结果：只改还在解析的（解析期间被删掉的不再建回来）。
func (s *Store) finishParse(ctx context.Context, receipt Receipt) error {
	draft, err := draftJSON(receipt.Draft)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE receipts SET status = ?, draft = ?, failure = ?, updated_at = ? WHERE id = ? AND status = ?`,
		receipt.Status, draft, receipt.Failure, receipt.UpdatedAt.Unix(), receipt.ID, StatusParsing)
	if err != nil {
		return fmt.Errorf("存小票的解析结果: %w", err)
	}
	return nil
}

// confirm 在一个事务里入账、把小票标成已入账：小票已经不是「解析好了」（另一个请求先确认了）时报 ErrState、什么都不改。
func (s *Store) confirm(ctx context.Context, receiptID string, draft Draft, expense Expense) error {
	data, err := draftJSON(&draft)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE receipts SET status = ?, draft = ?, expense_id = ?, updated_at = ? WHERE id = ? AND status = ?`,
		StatusConfirmed, data, expense.ID, expense.UpdatedAt.Unix(), receiptID, StatusReady)
	if err != nil {
		return fmt.Errorf("标小票已入账: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return ErrState
	}
	if err := putExpense(ctx, tx, expense); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteReceipt 删一张还没确认的小票（不是解析好 / 解析失败的报 ErrState）。
func (s *Store) deleteReceipt(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM receipts WHERE id = ? AND status IN (?, ?)`, id, StatusReady, StatusFailed)
	if err != nil {
		return fmt.Errorf("删小票: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return ErrState
	}
	return nil
}

// pendingCount 是这个人在解析的小票有几张。
func (s *Store) pendingCount(ctx context.Context, person string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM receipts WHERE person = ? AND status = ?`, person, StatusParsing).Scan(&count)
	return count, err
}

// staleReceipts 是 before 之前交的、已经不在解析的小票（清理用）。
func (s *Store) staleReceipts(ctx context.Context, before time.Time) ([]Receipt, error) {
	return s.receipts(ctx, `SELECT `+receiptColumns+` FROM receipts WHERE created_at < ? AND status != ?`, before.Unix(), StatusParsing)
}

// dropReceipt 删一条小票记录（清理用，不管状态）。
func (s *Store) dropReceipt(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM receipts WHERE id = ?`, id)
	return err
}

func draftJSON(draft *Draft) (any, error) {
	if draft == nil {
		return nil, nil
	}
	data, err := json.Marshal(draft)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func (s *Store) receipt(ctx context.Context, id string) (Receipt, error) {
	receipts, err := s.receipts(ctx, `SELECT `+receiptColumns+` FROM receipts WHERE id = ?`, id)
	if err != nil {
		return Receipt{}, err
	}
	if len(receipts) == 0 {
		return Receipt{}, ErrNotFound
	}
	return receipts[0], nil
}

// parsingReceipts 是还在解析的小票（服务端重启时没解析完的）。
func (s *Store) parsingReceipts(ctx context.Context) ([]Receipt, error) {
	return s.receipts(ctx, `SELECT `+receiptColumns+` FROM receipts WHERE status = ? ORDER BY created_at`, StatusParsing)
}

func (s *Store) receipts(ctx context.Context, query string, args ...any) ([]Receipt, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("读小票: %w", err)
	}
	defer rows.Close()
	var receipts []Receipt
	for rows.Next() {
		var receipt Receipt
		var draft, expense sql.NullString
		var created, updated int64
		if err := rows.Scan(&receipt.ID, &receipt.Person, &receipt.AttachmentID, &receipt.CategoryHint, &receipt.NoteHint, &receipt.Status,
			&draft, &receipt.Failure, &expense, &created, &updated); err != nil {
			return nil, fmt.Errorf("读小票: %w", err)
		}
		if draft.Valid {
			receipt.Draft = &Draft{}
			if err := json.Unmarshal([]byte(draft.String), receipt.Draft); err != nil {
				return nil, fmt.Errorf("小票 %s 的草稿解不开: %w", receipt.ID, err)
			}
		}
		receipt.ExpenseID = expense.String
		receipt.CreatedAt, receipt.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

// --- 账目

const expenseColumns = `id, person, merchant, spent_at, currency, total_micros, subtotal_micros, tax_micros, payment_method, category, note,
	items, attachment_id, created_at, updated_at`

// storedItem 是存进库的一行明细。
type storedItem struct {
	Name      string `json:"name"`
	Quantity  string `json:"quantity,omitempty"`
	UnitPrice *int64 `json:"unit_price_micros,omitempty"`
	Amount    int64  `json:"amount_micros"`
	TaxRate   string `json:"tax_rate,omitempty"`
}

// execer 是 *sql.DB 和 *sql.Tx 共有的：存账目在事务里、外都用同一个写法。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Store) putExpense(ctx context.Context, expense Expense) error {
	return putExpense(ctx, s.db, expense)
}

func putExpense(ctx context.Context, db execer, expense Expense) error {
	items := make([]storedItem, 0, len(expense.Items))
	for _, item := range expense.Items {
		stored := storedItem{Name: item.Name, Quantity: item.Quantity, Amount: item.Amount.Micros, TaxRate: item.TaxRate}
		if item.UnitPrice.Currency != "" {
			unit := item.UnitPrice.Micros
			stored.UnitPrice = &unit
		}
		items = append(items, stored)
	}
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	var subtotal, tax, attachment any
	if expense.Subtotal != nil {
		subtotal = expense.Subtotal.Micros
	}
	if expense.Tax != nil {
		tax = expense.Tax.Micros
	}
	if expense.AttachmentID != "" {
		attachment = expense.AttachmentID
	}
	_, err = db.ExecContext(ctx, `INSERT INTO expenses (`+expenseColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET merchant = excluded.merchant, spent_at = excluded.spent_at, currency = excluded.currency,
			total_micros = excluded.total_micros, subtotal_micros = excluded.subtotal_micros, tax_micros = excluded.tax_micros,
			payment_method = excluded.payment_method, category = excluded.category, note = excluded.note, items = excluded.items,
			updated_at = excluded.updated_at`,
		expense.ID, expense.Person, expense.Merchant, expense.SpentAt.Unix(), expense.Total.Currency, expense.Total.Micros, subtotal, tax,
		expense.PaymentMethod, expense.Category, expense.Note, string(data), attachment, expense.CreatedAt.Unix(), expense.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("存账目: %w", err)
	}
	return nil
}

func (s *Store) expense(ctx context.Context, id string) (Expense, error) {
	expenses, err := s.expenses(ctx, `SELECT `+expenseColumns+` FROM expenses WHERE id = ?`, id)
	if err != nil {
		return Expense{}, err
	}
	if len(expenses) == 0 {
		return Expense{}, ErrNotFound
	}
	return expenses[0], nil
}

// Filter 是列账目的条件：[From, To) 的时间段，分类、店名（空 = 不限；店名是整个对上）。
type Filter struct {
	From, To time.Time
	Category string
	Merchant string
}

func (s *Store) list(ctx context.Context, person string, filter Filter) ([]Expense, error) {
	query := `SELECT ` + expenseColumns + ` FROM expenses WHERE person = ? AND spent_at >= ? AND spent_at < ?`
	args := []any{person, filter.From.Unix(), filter.To.Unix()}
	if filter.Category != "" {
		query += ` AND category = ?`
		args = append(args, filter.Category)
	}
	if filter.Merchant != "" {
		query += ` AND merchant = ?`
		args = append(args, filter.Merchant)
	}
	return s.expenses(ctx, query+` ORDER BY spent_at DESC, created_at DESC`, args...)
}

// deleteExpense 删一笔账和它来自的那张小票的记录（同一个事务）。
func (s *Store) deleteExpense(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM receipts WHERE expense_id = ?`, id); err != nil {
		return fmt.Errorf("删账目的小票: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM expenses WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删账目: %w", err)
	}
	return tx.Commit()
}

func (s *Store) expenses(ctx context.Context, query string, args ...any) ([]Expense, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("读账目: %w", err)
	}
	defer rows.Close()
	var expenses []Expense
	for rows.Next() {
		var expense Expense
		var spent, total, created, updated int64
		var currency, items string
		var subtotal, tax sql.NullInt64
		var attachment sql.NullString
		if err := rows.Scan(&expense.ID, &expense.Person, &expense.Merchant, &spent, &currency, &total, &subtotal, &tax, &expense.PaymentMethod,
			&expense.Category, &expense.Note, &items, &attachment, &created, &updated); err != nil {
			return nil, fmt.Errorf("读账目: %w", err)
		}
		expense.SpentAt, expense.CreatedAt, expense.UpdatedAt = time.Unix(spent, 0).UTC(), time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
		expense.Total, expense.AttachmentID = money.Amount{Micros: total, Currency: currency}, attachment.String
		if subtotal.Valid {
			expense.Subtotal = &money.Amount{Micros: subtotal.Int64, Currency: currency}
		}
		if tax.Valid {
			expense.Tax = &money.Amount{Micros: tax.Int64, Currency: currency}
		}
		var stored []storedItem
		if err := json.Unmarshal([]byte(items), &stored); err != nil {
			return nil, fmt.Errorf("账目 %s 的明细解不开: %w", expense.ID, err)
		}
		for _, item := range stored {
			row := Item{Name: item.Name, Quantity: item.Quantity, Amount: money.Amount{Micros: item.Amount, Currency: currency}, TaxRate: item.TaxRate}
			if item.UnitPrice != nil {
				row.UnitPrice = money.Amount{Micros: *item.UnitPrice, Currency: currency}
			}
			expense.Items = append(expense.Items, row)
		}
		expenses = append(expenses, expense)
	}
	return expenses, rows.Err()
}

// --- 分类

func (s *Store) customCategories(ctx context.Context, person string) ([]Category, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, name FROM categories WHERE person = ? ORDER BY created_at`, person)
	if err != nil {
		return nil, fmt.Errorf("读分类: %w", err)
	}
	defer rows.Close()
	var categories []Category
	for rows.Next() {
		var category Category
		if err := rows.Scan(&category.Key, &category.Name); err != nil {
			return nil, err
		}
		categories = append(categories, category)
	}
	return categories, rows.Err()
}

func (s *Store) addCategory(ctx context.Context, person, key, name string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO categories (person, key, name, created_at) VALUES (?, ?, ?, ?)`, person, key, name, at.Unix())
	if err != nil {
		return fmt.Errorf("加分类: %w", err)
	}
	return nil
}

func (s *Store) deleteCategory(ctx context.Context, person, key string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM categories WHERE person = ? AND key = ?`, person, key)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return errors.Join(ErrNotFound, err)
	}
	return nil
}

// erasePerson 删掉这个人的全部账目、小票、自己加的分类，返回原图的编号（文件由调用方删）。
func (s *Store) erasePerson(ctx context.Context, person string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT attachment_id FROM receipts WHERE person = ? UNION
		SELECT attachment_id FROM expenses WHERE person = ? AND attachment_id IS NOT NULL`, person, person)
	if err != nil {
		return nil, fmt.Errorf("读原图: %w", err)
	}
	var images []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		images = append(images, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	err = sqliteschema.ErasePerson(ctx, s.db, person, `DELETE FROM receipts WHERE person = ?`, `DELETE FROM expenses WHERE person = ?`,
		`DELETE FROM categories WHERE person = ?`)
	return images, err
}
