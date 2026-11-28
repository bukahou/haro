// Package accounts 是 App 的账号和登录令牌（第 15 段）：一个服务端，多个账号，各账号的数据互相隔离。
//
// 账号下面挂两个「人」：本人、访客。对话、记忆、提醒等存储本来就按人存；每个请求先解析成账号（Authenticate），
// 之后只用这个账号的人取数据 —— 隔离只在这一处做，各接口不自己拼人名。管理员是谁由配置定（信任的根不放在运行时能改的地方），
// 管理员账号的人就是原来的声纹名和全家共用，现有数据不用改。
package accounts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/bukahou/haro/internal/money"

	"github.com/pressly/goose/v3"

	"github.com/bukahou/haro/internal/sqliteschema"
)

// Role 是账号的角色。
type Role string

// 角色。
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Status 是账号的状态。
type Status string

// 状态。
const (
	StatusActive          Status = "active"
	StatusPendingApproval Status = "pending_approval"
	StatusDisabled        Status = "disabled"
)

// JoinedVia 是账号怎么来的。
type JoinedVia string

// 怎么来的。
const (
	JoinedViaAdmin    JoinedVia = "admin"    // 配置里的管理员
	JoinedViaInvite   JoinedVia = "invite"   // 带邀请码登录
	JoinedViaApproval JoinedVia = "approval" // 登录后等管理员同意
	JoinedViaTerminal JoinedVia = "terminal" // 匿名终端
)

// Account 是一个账号。
type Account struct {
	ID              string
	Subject         string // akasha 的 sub；匿名终端的账号为空
	DisplayName     string
	Role            Role
	Status          Status
	JoinedVia       JoinedVia
	DefaultCurrency string // 空 = 用服务端的默认货币
	OwnerPerson     string // 本人（存储里的「人」）
	GuestPerson     string // 访客：这个账号下的共用对话
	CreatedAt       time.Time
	LastUsedAt      time.Time // 零值 = 还没用过
	AppVersion      string
	DeletionDueAt   time.Time    // 软删除后彻底删的时间；零值 = 没删
	MonthlyCap      money.Amount // 管理员给它定的月上限；零值 = 不设（管理员自己用全局的上限）
}

// 出错的种类（errors.Is 判断；接口按它给稳定的错误码）。
var (
	ErrNotFound        = errors.New("没有这个账号")
	ErrPendingApproval = errors.New("账号在等管理员同意")
	ErrDisabled        = errors.New("账号已停用")
	ErrInvalidToken    = errors.New("令牌无效")
	ErrTokenExpired    = errors.New("令牌过期了")
)

// Admin 是配置里的管理员：akasha 的 sub，和他原来的「人」（声纹名、全家共用）。
type Admin struct {
	Subject     string
	DisplayName string
	Person      string // 原来声纹登记的名字（现有数据按它存）
	GuestPerson string // 原来的全家共用（brain.SharedPerson）
}

// Store 是账号库（SQLite）。
type Store struct {
	db  *sql.DB
	now func() time.Time
	// anonymousCap 是新建的匿名账号的月上限（SetAnonymousMonthlyCap；零值 = 不设）
	anonymousCap money.Amount
}

// SetAnonymousMonthlyCap 定新建的匿名账号（终端批准、终端带邀请码登记时建的）的月上限；零值 = 不设（靠全局的上限兜底）。启动时调用一次。
func (s *Store) SetAnonymousMonthlyCap(cap money.Amount) { s.anonymousCap = cap }

//go:embed migrations/*.sql
var migrationFiles embed.FS

// versionTable 是账号库表结构的版本记在哪张表。
const versionTable = "goose_db_version_accounts"

// Open 打开（必要时创建）账号库并升到最新。path 为空 = 只在内存里（测试用）。
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sqliteschema.Open(ctx, path, migrationFiles,
		sqliteschema.Options{ForeignKeys: true, Migrate: []goose.ProviderOption{goose.WithTableName(versionTable)}})
	if err != nil {
		return nil, fmt.Errorf("打开账号库 %s: %w", path, err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close 关掉账号库。
func (s *Store) Close() error { return s.db.Close() }

// EnsureAdmin 按配置校正管理员（每次启动时）：这个 sub 的账号是管理员（没有就建，人用原来的声纹名和全家共用）；
// 别的账号即使以前是管理员，也改成普通账号 —— 谁是管理员只看配置。
func (s *Store) EnsureAdmin(ctx context.Context, admin Admin) (Account, error) {
	if admin.Subject == "" || admin.Person == "" {
		return Account{}, errors.New("管理员要有 akasha 的 sub 和原来的声纹名")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET role = ? WHERE role = ? AND (subject IS NULL OR subject != ?)`,
		RoleMember, RoleAdmin, admin.Subject); err != nil {
		return Account{}, err
	}
	account, err := scanAccount(tx.QueryRowContext(ctx, selectAccount+` WHERE subject = ?`, admin.Subject))
	switch {
	case errors.Is(err, ErrNotFound):
		id, err := newID()
		if err != nil {
			return Account{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (id, subject, display_name, role, status, joined_via, owner_person,
			guest_person, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, admin.Subject, admin.DisplayName, RoleAdmin,
			StatusActive, JoinedViaAdmin, admin.Person, admin.GuestPerson, s.now().Unix()); err != nil {
			return Account{}, fmt.Errorf("建管理员账号: %w", err)
		}
	case err != nil:
		return Account{}, err
	default:
		samePersons := account.OwnerPerson == admin.Person && account.GuestPerson == admin.GuestPerson
		switch {
		case samePersons:
		case account.Status == StatusPendingApproval:
			// 管理员先登录一次（从日志里拿到自己的 sub）才写进配置：那次登录建的是等同意的账号，从来没用过、没有数据，
			// 直接换成原来的人
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET owner_person = ?, guest_person = ?, joined_via = ? WHERE id = ?`,
				admin.Person, admin.GuestPerson, JoinedViaAdmin, account.ID); err != nil {
				return Account{}, fmt.Errorf("把等同意的账号换成管理员: %w", err)
			}
		default:
			return Account{}, fmt.Errorf("管理员账号原来对应的人是 %q / %q，配置里是 %q / %q：人名改了的话数据就对不上了，先查清楚",
				account.OwnerPerson, account.GuestPerson, admin.Person, admin.GuestPerson)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET role = ?, status = ? WHERE id = ?`, RoleAdmin, StatusActive, account.ID); err != nil {
			return Account{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	return s.BySubject(ctx, admin.Subject)
}

// ResolveLogin 是 akasha 登录回来后找账号：有就用（停用的、在等同意的报对应的错；带的邀请码不用掉）。
// 没有时：带着有效的邀请码 → 建一个正常的账号（来源 = 邀请），码标成用过（同一个事务）；带了无效 / 过期 / 用过的码 →
// ErrInviteInvalid，不建账号；没带码 → 建一个等管理员同意的账号，报 ErrPendingApproval。
func (s *Store) ResolveLogin(ctx context.Context, subject, displayName, invite string) (Account, error) {
	account, err := s.BySubject(ctx, subject)
	switch {
	case errors.Is(err, ErrNotFound) && invite != "":
		return s.joinByInvite(ctx, subject, displayName, invite)
	case errors.Is(err, ErrNotFound):
		if _, err := s.create(ctx, s.db, subject, displayName, StatusPendingApproval, JoinedViaApproval); err != nil {
			return Account{}, err
		}
		return Account{}, ErrPendingApproval
	case err != nil:
		return Account{}, err
	}
	return account, usable(account)
}

// usable 判断账号现在能不能用。
func usable(account Account) error {
	switch account.Status {
	case StatusActive:
		return nil
	case StatusPendingApproval:
		return ErrPendingApproval
	default:
		return ErrDisabled
	}
}

// execer 是 *sql.DB 和 *sql.Tx 共有的：建账号在事务里、外都用同一个写法。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// create 建一个普通账号，返回它的编号；它的人用账号编号生成（不会和别的账号、原来的声纹名重名）。
func (s *Store) create(ctx context.Context, db execer, subject, displayName string, status Status, via JoinedVia) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	var nullableSubject any
	if subject != "" {
		nullableSubject = subject
	}
	owner, guest := PersonsOf(id)
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts (id, subject, display_name, role, status, joined_via, owner_person,
		guest_person, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, nullableSubject, displayName, RoleMember, status, via,
		owner, guest, s.now().Unix()); err != nil {
		return "", fmt.Errorf("建账号: %w", err)
	}
	return id, nil
}

// ByID 按编号找账号。
func (s *Store) ByID(ctx context.Context, id string) (Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, selectAccount+` WHERE id = ?`, id))
}

// BySubject 按 akasha 的 sub 找账号。
func (s *Store) BySubject(ctx context.Context, subject string) (Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, selectAccount+` WHERE subject = ?`, subject))
}

// List 列出全部账号（管理员看运维信息用；先建的在前）。
func (s *Store) List(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, selectAccount+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Account
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, account)
	}
	return list, rows.Err()
}

// Touch 记下账号刚用过（和用的 App 版本；空 = 不改）。
func (s *Store) Touch(ctx context.Context, id, appVersion string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET last_used_at = ?, app_version = COALESCE(NULLIF(?, ''), app_version) WHERE id = ?`,
		s.now().Unix(), appVersion, id)
	return err
}

// UpdateProfile 改显示名、默认货币（空 = 不改）。
func (s *Store) UpdateProfile(ctx context.Context, id, displayName, currency string) (Account, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET display_name = COALESCE(NULLIF(?, ''), display_name),
		default_currency = COALESCE(NULLIF(?, ''), default_currency) WHERE id = ?`, displayName, currency, id)
	if err != nil {
		return Account{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return Account{}, ErrNotFound
	}
	return s.ByID(ctx, id)
}

const selectAccount = `SELECT id, COALESCE(subject, ''), display_name, role, status, joined_via, default_currency, owner_person,
	guest_person, created_at, COALESCE(last_used_at, 0), COALESCE(app_version, ''), COALESCE(deletion_due_at, 0),
	COALESCE(monthly_cap_micros, 0), COALESCE(monthly_cap_currency, '') FROM accounts`

type rowScanner interface{ Scan(dest ...any) error }

func scanAccount(row rowScanner) (Account, error) {
	var account Account
	var created, lastUsed, deletionDue int64
	err := row.Scan(&account.ID, &account.Subject, &account.DisplayName, &account.Role, &account.Status, &account.JoinedVia,
		&account.DefaultCurrency, &account.OwnerPerson, &account.GuestPerson, &created, &lastUsed, &account.AppVersion, &deletionDue,
		&account.MonthlyCap.Micros, &account.MonthlyCap.Currency)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, err
	}
	account.CreatedAt = time.Unix(created, 0).UTC()
	if lastUsed > 0 {
		account.LastUsedAt = time.Unix(lastUsed, 0).UTC()
	}
	if deletionDue > 0 {
		account.DeletionDueAt = time.Unix(deletionDue, 0).UTC()
	}
	return account, nil
}

// newID 是随机编号（128 位，十六进制）。
func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
