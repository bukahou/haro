package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 邀请码的规矩（定值）。
const (
	// inviteCodeLength 是邀请码几个字（字表和配对码一样；显示时每 4 个字加「-」）：31^12 ≈ 2^59，有时效、用一次、登录限流
	inviteCodeLength = 12
	// DefaultInviteTTL、MaxInviteTTL：邀请码默认多久过期、最长多久。
	DefaultInviteTTL = 7 * 24 * time.Hour
	MaxInviteTTL     = 30 * 24 * time.Hour
	// MaxInviteNoteRunes 是备注最多几个字。
	MaxInviteNoteRunes = 200
)

// 邀请码的状态。
const (
	InviteUnused  = "unused"
	InviteUsed    = "used"
	InviteExpired = "expired"
	InviteRevoked = "revoked"
)

// 邀请码的错误。
var (
	ErrInviteInvalid  = errors.New("邀请码不对、过期了、用过了或者撤销了")
	ErrInviteNotFound = errors.New("没有这个邀请码（或者已经用过、撤销了）")
)

// Invite 是一个邀请码（码本身只在生成时给一次，库里只有它的 SHA-256）。
type Invite struct {
	ID        string
	Note      string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time // 零值 = 没用
	UsedBy    string    // 用它建的账号；没用、或者那个账号已经彻底删了时为空
	RevokedAt time.Time // 零值 = 没撤销
}

// Status 是邀请码现在的状态（按 now 判断过没过期）。
func (i Invite) Status(now time.Time) string {
	switch {
	case !i.RevokedAt.IsZero():
		return InviteRevoked
	case !i.UsedAt.IsZero():
		return InviteUsed
	case !now.Before(i.ExpiresAt):
		return InviteExpired
	}
	return InviteUnused
}

// CreateInvite 生成一个邀请码（管理员 createdBy 生成，ttl 后过期，超过 MaxInviteTTL 或不到 1 小时的报错），返回邀请码和码本身（显示形式）。
func (s *Store) CreateInvite(ctx context.Context, createdBy, note string, ttl time.Duration) (Invite, string, error) {
	if ttl < time.Hour || ttl > MaxInviteTTL {
		return Invite{}, "", fmt.Errorf("邀请码的有效期要在 1 小时到 %v 之间", MaxInviteTTL)
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > MaxInviteNoteRunes {
		return Invite{}, "", fmt.Errorf("备注最多 %d 个字", MaxInviteNoteRunes)
	}
	id, err := newID()
	if err != nil {
		return Invite{}, "", err
	}
	code, err := randomCode(inviteCodeLength)
	if err != nil {
		return Invite{}, "", err
	}
	now := s.now().UTC().Truncate(time.Second)
	invite := Invite{ID: id, Note: note, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO invites (id, code_hash, note, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, hashToken(code), note, createdBy, now.Unix(), invite.ExpiresAt.Unix()); err != nil {
		return Invite{}, "", fmt.Errorf("存邀请码: %w", err)
	}
	return invite, FormatPairCode(code), nil
}

// Invites 是全部邀请码（新的在前）。
func (s *Store) Invites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, note, created_at, expires_at, COALESCE(used_at, 0), COALESCE(used_by, ''),
		COALESCE(revoked_at, 0) FROM invites ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("读邀请码: %w", err)
	}
	defer rows.Close()
	var invites []Invite
	for rows.Next() {
		var invite Invite
		var created, expires, used, revoked int64
		if err := rows.Scan(&invite.ID, &invite.Note, &created, &expires, &used, &invite.UsedBy, &revoked); err != nil {
			return nil, err
		}
		invite.CreatedAt, invite.ExpiresAt = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
		if used > 0 {
			invite.UsedAt = time.Unix(used, 0).UTC()
		}
		if revoked > 0 {
			invite.RevokedAt = time.Unix(revoked, 0).UTC()
		}
		invites = append(invites, invite)
	}
	return invites, rows.Err()
}

// RevokeInvite 撤销一个还没用的邀请码（用过、撤销过的报 ErrInviteNotFound）。
func (s *Store) RevokeInvite(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE invites SET revoked_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL`,
		s.now().Unix(), id)
	if err != nil {
		return fmt.Errorf("撤销邀请码: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return ErrInviteNotFound
	}
	return nil
}

// joinByInvite 用邀请码建一个正常的账号：码有效才建，码标成用过（同一个事务：同一个码同时用两次只成一次）。
func (s *Store) joinByInvite(ctx context.Context, subject, displayName, code string) (Account, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := s.create(ctx, tx, subject, displayName, StatusActive, JoinedViaInvite)
	if err != nil {
		return Account{}, err
	}
	if err := s.useInvite(ctx, tx, code, id); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, err
	}
	return s.ByID(ctx, id)
}

// useInvite 在事务 tx 里用掉一个邀请码（记下用它建的账号）：码不对、过期、用过、撤销了报 ErrInviteInvalid（调用方回滚）。
func (s *Store) useInvite(ctx context.Context, tx *sql.Tx, code, accountID string) error {
	result, err := tx.ExecContext(ctx, `UPDATE invites SET used_at = ?, used_by = ? WHERE code_hash = ? AND used_at IS NULL AND
		revoked_at IS NULL AND expires_at > ?`, s.now().Unix(), accountID, hashToken(normalizePairCode(code)), s.now().Unix())
	if err != nil {
		return fmt.Errorf("用掉邀请码: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return ErrInviteInvalid
	}
	return nil
}
