package accounts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 终端的登记照 RFC 8628（设备授权）的样子：终端拿配对码给人看、拿轮询凭据定时问结果；
// 人在 App 里输入配对码 = 绑到自己的账号（已登录、已批准的账号配对，等于批准），管理员批准 = 归一个新的匿名账号。
const (
	// RegistrationTTL 是配对码多久有效。
	RegistrationTTL = 10 * time.Minute
	// RegistrationPollInterval 是终端至少隔多久问一次结果。
	RegistrationPollInterval = 5 * time.Second
	// pairCodeLength 是配对码几个字（显示时每 4 个字中间加一个「-」）。
	pairCodeLength = 8
	// pairCodeAlphabet 是配对码用的字：大写字母和数字，去掉容易看混、念混的（0 O 1 I L）。
	pairCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
)

// 登记、终端的错误（App 的接口和终端按它们给状态，⛔ 不看文字）。
var (
	ErrRegistrationNotFound = errors.New("没有这个登记")
	ErrPairCodeInvalid      = errors.New("配对码不对")
	ErrPairCodeExpired      = errors.New("配对码过期了")
	ErrPairCodeUsed         = errors.New("配对码已经用过了")
	ErrInvalidCredential    = errors.New("终端的凭证无效（可能已在 App 里移除）")
	ErrTerminalNotFound     = errors.New("没有这台终端")
)

// RegistrationStatus 是登记的结果。
type RegistrationStatus string

// 登记的结果（终端轮询时看到的）。
const (
	RegistrationPending  RegistrationStatus = "pending"
	RegistrationApproved RegistrationStatus = "approved"
	RegistrationRejected RegistrationStatus = "rejected"
	RegistrationExpired  RegistrationStatus = "expired"
)

// Registration 是刚开始的登记：配对码给人看，轮询凭据终端自己留着；短编号终端也显示（和管理员看到的对得上）。
type Registration struct {
	ID        string
	ShortID   string
	Code      string // 带「-」的显示形式
	PollToken string
	ExpiresAt time.Time
}

// PendingRegistration 是等着配对 / 批准的登记（给管理员看）。
type PendingRegistration struct {
	ID, ShortID, Name, Version, Remote string
	CreatedAt, ExpiresAt               time.Time
}

// PollResult 是终端问到的登记结果；批准了时带着设备凭证（只交出这一次）。
type PollResult struct {
	Status     RegistrationStatus
	TerminalID string
	Credential string
}

// Terminal 是一台终端。
type Terminal struct {
	ID, AccountID, Name, Version string
	LastRemote                   string // 最近一次从哪里连来的（没连过时为空）
	CreatedAt                    time.Time
	LastSeenAt                   *time.Time
	CredentialIssued             bool // 终端已经取走了凭证
}

// StartRegistration 开始登记一台终端：name、version 是终端报的，remote 是从哪里连来的。
func (s *Store) StartRegistration(ctx context.Context, name, version, remote string) (Registration, error) {
	id, err := newID()
	if err != nil {
		return Registration{}, err
	}
	code, err := newPairCode()
	if err != nil {
		return Registration{}, err
	}
	poll, err := newToken()
	if err != nil {
		return Registration{}, err
	}
	now := s.now()
	expires := now.Add(RegistrationTTL)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO terminal_registrations (id, code_hash, poll_hash, name, version, remote, created_at,
		expires_at, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, hashToken(code), hashToken(poll), name, version, remote,
		now.Unix(), expires.Unix(), RegistrationPending); err != nil {
		return Registration{}, fmt.Errorf("登记终端: %w", err)
	}
	return Registration{ID: id, ShortID: shortID(id), Code: FormatPairCode(code), PollToken: poll,
		ExpiresAt: expires.UTC().Truncate(time.Second)}, nil
}

// shortID 是登记的短编号（编号的前 6 位、大写）。
func shortID(id string) string { return strings.ToUpper(id[:min(6, len(id))]) }

// newPairCode 是随机配对码（不带「-」）。
func newPairCode() (string, error) {
	buf := make([]byte, pairCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	code := make([]byte, pairCodeLength)
	for i, b := range buf { // 256 不是字表长度的整数倍：取模有极小的偏差，对 10 分钟、用一次的配对码无所谓
		code[i] = pairCodeAlphabet[int(b)%len(pairCodeAlphabet)]
	}
	return string(code), nil
}

// FormatPairCode 是配对码的显示形式（每 4 个字中间加「-」）。
func FormatPairCode(code string) string {
	if len(code) <= 4 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// normalizePairCode 是输入的配对码去掉「-」和空白、转大写（手输时常见的写法都认）。
func normalizePairCode(input string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			return -1
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		}
		return r
	}, input)
}

// PairRegistration 把配对码对应的终端绑到 accountID（App 里输入配对码）：建好终端，等终端来取凭证。
// 配对码不对、过期了、用过了分别是 ErrPairCodeInvalid、ErrPairCodeExpired、ErrPairCodeUsed。
func (s *Store) PairRegistration(ctx context.Context, accountID, code string) (Terminal, error) {
	terminal, err := s.approve(ctx, `code_hash = ?`, hashToken(normalizePairCode(code)), func(*sql.Tx) (string, error) { return accountID, nil })
	switch {
	case errors.Is(err, errRegistrationExpired):
		return Terminal{}, ErrPairCodeExpired
	case errors.Is(err, errRegistrationDecided):
		return Terminal{}, ErrPairCodeUsed
	case errors.Is(err, ErrRegistrationNotFound):
		return Terminal{}, ErrPairCodeInvalid
	}
	return terminal, err
}

// approve 找不到能批准的登记时的两种原因（PairRegistration 分开报；管理员批准时都当成没有）。
var (
	errRegistrationExpired = fmt.Errorf("%w：过期了", ErrRegistrationNotFound)
	errRegistrationDecided = fmt.Errorf("%w：已经批准或拒绝过了", ErrRegistrationNotFound)
)

// ApproveRegistration 是管理员批准一台新终端：给它建一个新的匿名账号。
func (s *Store) ApproveRegistration(ctx context.Context, registrationID string) (Terminal, error) {
	return s.approve(ctx, `id = ?`, registrationID, func(tx *sql.Tx) (string, error) {
		id, err := newID()
		if err != nil {
			return "", err
		}
		owner, guest := PersonsOf(id)
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (id, display_name, role, status, joined_via, owner_person, guest_person,
			created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, "", RoleMember, StatusActive, JoinedViaTerminal, owner, guest, s.now().Unix()); err != nil {
			return "", fmt.Errorf("建匿名账号: %w", err)
		}
		return id, nil
	})
}

// approve 找到还在等的登记（没过期），交给 accountFor 定归哪个账号，建好终端、把登记标成已批准。
func (s *Store) approve(ctx context.Context, where string, key any, accountFor func(*sql.Tx) (string, error)) (Terminal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Terminal{}, err
	}
	defer tx.Rollback()
	var registrationID, name string
	var status RegistrationStatus
	var expires int64
	var version sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, name, version, status, expires_at FROM terminal_registrations WHERE `+where, key).
		Scan(&registrationID, &name, &version, &status, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Terminal{}, ErrRegistrationNotFound
	case err != nil:
		return Terminal{}, fmt.Errorf("找登记: %w", err)
	case status != RegistrationPending:
		return Terminal{}, errRegistrationDecided
	case expires <= s.now().Unix():
		return Terminal{}, errRegistrationExpired
	}
	accountID, err := accountFor(tx)
	if err != nil {
		return Terminal{}, err
	}
	terminalID, err := newID()
	if err != nil {
		return Terminal{}, err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO terminals (id, account_id, name, created_at, version) VALUES (?, ?, ?, ?, ?)`,
		terminalID, accountID, name, now.Unix(), version); err != nil {
		return Terminal{}, fmt.Errorf("建终端: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE terminal_registrations SET status = ?, terminal_id = ? WHERE id = ?`,
		RegistrationApproved, terminalID, registrationID); err != nil {
		return Terminal{}, fmt.Errorf("标记登记: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Terminal{}, err
	}
	return Terminal{ID: terminalID, AccountID: accountID, Name: name, Version: version.String, CreatedAt: now.UTC().Truncate(time.Second)}, nil
}

// RejectRegistration 是管理员拒绝一台新终端。
func (s *Store) RejectRegistration(ctx context.Context, registrationID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE terminal_registrations SET status = ? WHERE id = ? AND status = ?`,
		RegistrationRejected, registrationID, RegistrationPending)
	if err != nil {
		return fmt.Errorf("拒绝登记: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return ErrRegistrationNotFound
	}
	return nil
}

// PendingRegistrations 是还在等配对 / 批准的登记（没过期的；新的在前）。
func (s *Store) PendingRegistrations(ctx context.Context) ([]PendingRegistration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, version, remote, created_at, expires_at FROM terminal_registrations
		WHERE status = ? AND expires_at > ? ORDER BY created_at DESC`, RegistrationPending, s.now().Unix())
	if err != nil {
		return nil, fmt.Errorf("读登记: %w", err)
	}
	defer rows.Close()
	var pending []PendingRegistration
	for rows.Next() {
		var registration PendingRegistration
		var version, remote sql.NullString
		var created, expires int64
		if err := rows.Scan(&registration.ID, &registration.Name, &version, &remote, &created, &expires); err != nil {
			return nil, fmt.Errorf("读登记: %w", err)
		}
		registration.Version, registration.Remote, registration.ShortID = version.String, remote.String, shortID(registration.ID)
		registration.CreatedAt, registration.ExpiresAt = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
		pending = append(pending, registration)
	}
	return pending, rows.Err()
}

// PollRegistration 是终端问登记的结果。批准了：生成设备凭证（只交出这一次）、登记删掉；拒绝了、过期了：登记删掉。
func (s *Store) PollRegistration(ctx context.Context, pollToken string) (PollResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PollResult{}, err
	}
	defer tx.Rollback()
	var id string
	var status RegistrationStatus
	var expires int64
	var terminalID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, status, expires_at, terminal_id FROM terminal_registrations WHERE poll_hash = ?`,
		hashToken(pollToken)).Scan(&id, &status, &expires, &terminalID)
	if errors.Is(err, sql.ErrNoRows) {
		return PollResult{}, ErrRegistrationNotFound
	} else if err != nil {
		return PollResult{}, fmt.Errorf("找登记: %w", err)
	}
	result := PollResult{Status: status}
	switch {
	case status == RegistrationPending && expires > s.now().Unix():
		return result, nil
	case status == RegistrationPending:
		result.Status = RegistrationExpired
	case status == RegistrationApproved:
		credential, err := newToken()
		if err != nil {
			return PollResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE terminals SET credential_hash = ? WHERE id = ? AND credential_hash IS NULL`,
			hashToken(credential), terminalID.String); err != nil {
			return PollResult{}, fmt.Errorf("发设备凭证: %w", err)
		}
		result.TerminalID, result.Credential = terminalID.String, credential
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM terminal_registrations WHERE id = ?`, id); err != nil {
		return PollResult{}, fmt.Errorf("删登记: %w", err)
	}
	return result, tx.Commit()
}

// AuthenticateTerminal 验终端连进来时带的设备凭证：返回终端和它的账号（账号要能用）。
func (s *Store) AuthenticateTerminal(ctx context.Context, credential string) (Terminal, Account, error) {
	terminal, err := scanTerminal(s.db.QueryRowContext(ctx, selectTerminal+` WHERE credential_hash = ?`, hashToken(credential)))
	if errors.Is(err, ErrTerminalNotFound) {
		return Terminal{}, Account{}, ErrInvalidCredential
	} else if err != nil {
		return Terminal{}, Account{}, err
	}
	account, err := s.ByID(ctx, terminal.AccountID)
	if err != nil {
		return Terminal{}, Account{}, err
	}
	return terminal, account, usable(account)
}

// TouchTerminal 记下终端最近一次连进来的时间、版本、从哪里来的。
func (s *Store) TouchTerminal(ctx context.Context, terminalID, version, remote string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE terminals SET last_seen_at = ?, version = ?, last_remote = ? WHERE id = ?`,
		s.now().Unix(), version, remote, terminalID)
	return err
}

// TerminalsOf 是一个账号的终端（先建的在前）。
func (s *Store) TerminalsOf(ctx context.Context, accountID string) ([]Terminal, error) {
	rows, err := s.db.QueryContext(ctx, selectTerminal+` WHERE account_id = ? ORDER BY created_at`, accountID)
	if err != nil {
		return nil, fmt.Errorf("读终端: %w", err)
	}
	defer rows.Close()
	var terminals []Terminal
	for rows.Next() {
		terminal, err := scanTerminal(rows)
		if err != nil {
			return nil, err
		}
		terminals = append(terminals, terminal)
	}
	return terminals, rows.Err()
}

// TerminalOf 是一个账号的一台终端（不是这个账号的当成没有）。
func (s *Store) TerminalOf(ctx context.Context, accountID, terminalID string) (Terminal, error) {
	return scanTerminal(s.db.QueryRowContext(ctx, selectTerminal+` WHERE id = ? AND account_id = ?`, terminalID, accountID))
}

// RenameTerminal 改一台终端的名字（只能改自己账号的）。
func (s *Store) RenameTerminal(ctx context.Context, accountID, terminalID, name string) (Terminal, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE terminals SET name = ? WHERE id = ? AND account_id = ?`, name, terminalID, accountID)
	if err != nil {
		return Terminal{}, fmt.Errorf("改终端的名字: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return Terminal{}, ErrTerminalNotFound
	}
	return s.TerminalOf(ctx, accountID, terminalID)
}

// RemoveTerminal 移除一台终端（只能移除自己账号的）：凭证跟着作废。
func (s *Store) RemoveTerminal(ctx context.Context, accountID, terminalID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM terminals WHERE id = ? AND account_id = ?`, terminalID, accountID)
	if err != nil {
		return fmt.Errorf("移除终端: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return ErrTerminalNotFound
	}
	return nil
}

// unclaimedTTL 是批准了、终端却一直没来取凭证的登记留多久（之后终端要重新登记；建好的终端在 App 里显示「还没连上过」，可以移除）。
const unclaimedTTL = 24 * time.Hour

// PruneRegistrations 清掉终端不再来问的登记：过期、被拒绝的，和批准了却一直没来取凭证的。
func (s *Store) PruneRegistrations(ctx context.Context) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, `DELETE FROM terminal_registrations WHERE (status != ? AND expires_at < ?) OR (status = ? AND expires_at < ?)`,
		RegistrationApproved, now.Add(-RegistrationTTL).Unix(), RegistrationApproved, now.Add(-unclaimedTTL).Unix())
	return err
}

const selectTerminal = `SELECT id, account_id, name, version, last_remote, created_at, last_seen_at, credential_hash IS NOT NULL FROM terminals`

func scanTerminal(row rowScanner) (Terminal, error) {
	var terminal Terminal
	var version, remote sql.NullString
	var created int64
	var seen sql.NullInt64
	err := row.Scan(&terminal.ID, &terminal.AccountID, &terminal.Name, &version, &remote, &created, &seen, &terminal.CredentialIssued)
	if errors.Is(err, sql.ErrNoRows) {
		return Terminal{}, ErrTerminalNotFound
	} else if err != nil {
		return Terminal{}, fmt.Errorf("读终端: %w", err)
	}
	terminal.Version, terminal.LastRemote, terminal.CreatedAt = version.String, remote.String, time.Unix(created, 0).UTC()
	if seen.Valid {
		at := time.Unix(seen.Int64, 0).UTC()
		terminal.LastSeenAt = &at
	}
	return terminal, nil
}
