package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bukahou/haro/internal/money"
)

// DeletionGrace 是软删除以后多久彻底删（v11 第 9 节第 7 项：先软删除，30 天后彻底删）。
const DeletionGrace = 30 * 24 * time.Hour

// 管理账号的错误。
var (
	ErrAccountState = errors.New("这个账号现在不能这样做")
	ErrIsAdmin      = errors.New("管理员的账号不能停用、删除")
)

// Approve 同意一个在等的账号（等待批准 → 正常）。
func (s *Store) Approve(ctx context.Context, id string) (Account, error) {
	return s.updateAccount(ctx, id, `status = ?`, []any{StatusActive}, `status = ?`, []any{StatusPendingApproval})
}

// Disable 停用一个账号（正常 / 等待批准 → 停用）：作废它的全部登录令牌。终端的连接由调用方断开。
func (s *Store) Disable(ctx context.Context, id string) (Account, error) {
	return s.updateAccount(ctx, id, `status = ?, disabled_at = ?`, []any{StatusDisabled, s.now().Unix()},
		`status IN (?, ?) AND role != ?`, []any{StatusActive, StatusPendingApproval, RoleAdmin})
}

// Enable 恢复一个停用了的账号（停用 → 正常）；软删除了、还没到期的也恢复（撤销删除）。
func (s *Store) Enable(ctx context.Context, id string) (Account, error) {
	return s.updateAccount(ctx, id, `status = ?, disabled_at = NULL, deletion_due_at = NULL`, []any{StatusActive},
		`status = ?`, []any{StatusDisabled})
}

// ScheduleDeletion 软删除一个账号：停用、DeletionGrace 以后彻底删（PurgeDue）。已经软删除的不改到期时间。
func (s *Store) ScheduleDeletion(ctx context.Context, id string) (Account, error) {
	now := s.now()
	return s.updateAccount(ctx, id, `status = ?, disabled_at = COALESCE(disabled_at, ?), deletion_due_at = COALESCE(deletion_due_at, ?)`,
		[]any{StatusDisabled, now.Unix(), now.Add(DeletionGrace).Unix()}, `role != ?`, []any{RoleAdmin})
}

// updateAccount 按条件改账号（条件不满足：管理员报 ErrIsAdmin，别的报 ErrAccountState），停用的作废全部登录令牌（同一个事务）。
func (s *Store) updateAccount(ctx context.Context, id, set string, setArgs []any, where string, whereArgs []any) (Account, error) {
	account, err := s.ByID(ctx, id)
	if err != nil {
		return Account{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback() }()
	args := append(append(append([]any{}, setArgs...), id), whereArgs...)
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET `+set+` WHERE id = ? AND `+where, args...)
	if err != nil {
		return Account{}, fmt.Errorf("改账号: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Account{}, err
	} else if affected == 0 {
		if account.Role == RoleAdmin {
			return Account{}, ErrIsAdmin
		}
		return Account{}, ErrAccountState
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE account_id = ? AND revoked_at IS NULL AND
		(SELECT status FROM accounts WHERE id = ?) = ?`, s.now().Unix(), id, id, StatusDisabled); err != nil {
		return Account{}, fmt.Errorf("作废登录令牌: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	return s.ByID(ctx, id)
}

// DueForDeletion 是软删除到期（到 now 为止）的账号。
func (s *Store) DueForDeletion(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, selectAccount+` WHERE deletion_due_at IS NOT NULL AND deletion_due_at <= ? AND role != ?`,
		s.now().Unix(), RoleAdmin)
	if err != nil {
		return nil, fmt.Errorf("读到期的账号: %w", err)
	}
	defer rows.Close()
	var due []Account
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		due = append(due, account)
	}
	return due, rows.Err()
}

// Purge 彻底删掉一个软删除到期的账号（账号库里的令牌、资料、家、终端随它删）。别的存储里的数据由调用方先删。
func (s *Store) Purge(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ? AND deletion_due_at IS NOT NULL AND deletion_due_at <= ? AND role != ?`,
		id, s.now().Unix(), RoleAdmin)
	if err != nil {
		return fmt.Errorf("删账号: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return ErrAccountState
	}
	return nil
}

// SetMonthlyCap 定一个账号的月上限（cap 为零值 = 不设）。管理员的账号用全局的上限，报 ErrIsAdmin。
func (s *Store) SetMonthlyCap(ctx context.Context, id string, cap money.Amount) (Account, error) {
	if cap.Micros < 0 {
		return Account{}, errors.New("上限不能是负数")
	}
	var micros, currency any
	if cap.Micros > 0 {
		micros, currency = cap.Micros, cap.Currency
	}
	return s.updateAccount(ctx, id, `monthly_cap_micros = ?, monthly_cap_currency = ?`, []any{micros, currency}, `role != ?`, []any{RoleAdmin})
}

// MonthlyCapOf 实现 spending.AccountCaps：账号的月上限（没有这个账号、没定的为零值）。
func (s *Store) MonthlyCapOf(ctx context.Context, id string) (money.Amount, error) {
	account, err := s.ByID(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return money.Amount{}, nil
	}
	if err != nil {
		return money.Amount{}, err
	}
	return account.MonthlyCap, nil
}

// IdleAnonymous 是 before 之前就再没连过的匿名账号（终端建的、没有 akasha 账号）：它的终端都没在 before 之后连过
// （没连过的按建的时间算；一台终端都没有的按账号建的时间）。已经软删除的不算。
func (s *Store) IdleAnonymous(ctx context.Context, before time.Time) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, selectAccount+` WHERE joined_via = ? AND subject IS NULL AND deletion_due_at IS NULL AND role != ?
		AND created_at < ? AND NOT EXISTS (SELECT 1 FROM terminals WHERE terminals.account_id = accounts.id
			AND COALESCE(terminals.last_seen_at, terminals.created_at) >= ?)`, JoinedViaTerminal, RoleAdmin, before.Unix(), before.Unix())
	if err != nil {
		return nil, fmt.Errorf("读久不连接的匿名账号: %w", err)
	}
	defer rows.Close()
	var idle []Account
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		idle = append(idle, account)
	}
	return idle, rows.Err()
}
