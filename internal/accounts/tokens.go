package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// 令牌的有效期：访问令牌短、刷新令牌长并且用一次就换新（OAuth 2.0 安全实践的通常做法）。
const (
	AccessTokenTTL  = time.Hour
	RefreshTokenTTL = 90 * 24 * time.Hour
)

// 令牌的种类（tokens.kind）。
const (
	kindAccess  = "access"
	kindRefresh = "refresh"
)

// TokenPair 是发给 App 的一对令牌（明文只在这里出现一次，库里只存散列）。
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    time.Duration // 访问令牌还有多久过期
}

// IssueTokens 给账号发一对新令牌（登录成功时）：开一串新的（family）。
func (s *Store) IssueTokens(ctx context.Context, accountID string) (TokenPair, error) {
	family, err := newID()
	if err != nil {
		return TokenPair{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TokenPair{}, err
	}
	defer func() { _ = tx.Rollback() }()
	pair, err := s.insertPair(ctx, tx, accountID, family)
	if err != nil {
		return TokenPair{}, err
	}
	return pair, tx.Commit()
}

// Authenticate 用访问令牌找账号：令牌无效、过期、被作废，或账号不能用（等同意、停用）时报对应的错。
func (s *Store) Authenticate(ctx context.Context, accessToken string) (Account, error) {
	var accountID string
	var expires int64
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT account_id, expires_at, revoked_at FROM tokens WHERE hash = ? AND kind = ?`,
		hashToken(accessToken), kindAccess).Scan(&accountID, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) || revoked.Valid {
		return Account{}, ErrInvalidToken
	}
	if err != nil {
		return Account{}, err
	}
	if s.now().Unix() >= expires {
		return Account{}, ErrTokenExpired
	}
	account, err := s.ByID(ctx, accountID)
	if err != nil {
		return Account{}, err
	}
	return account, usable(account)
}

// Refresh 用刷新令牌换一对新的：旧的刷新令牌标成用过（同一张并发刷新只有一方成功，按影响行数判断）。
// 已经用过的刷新令牌又被拿来用 —— 说明被盗或被复制了 —— 这一串令牌全部作废，要重新登录。
func (s *Store) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TokenPair{}, err
	}
	defer func() { _ = tx.Rollback() }()
	hash := hashToken(refreshToken)
	var accountID, family string
	var expires int64
	var used, revoked sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT account_id, family, expires_at, used_at, revoked_at FROM tokens WHERE hash = ? AND kind = ?`,
		hash, kindRefresh).Scan(&accountID, &family, &expires, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) || revoked.Valid {
		return TokenPair{}, ErrInvalidToken
	}
	if err != nil {
		return TokenPair{}, err
	}
	now := s.now().Unix()
	if used.Valid { // 重复使用：整串作废（在这个事务外也要生效，所以单独提交）
		_ = tx.Rollback()
		if err := s.revokeFamily(ctx, family); err != nil {
			return TokenPair{}, err
		}
		return TokenPair{}, ErrInvalidToken
	}
	if now >= expires {
		return TokenPair{}, ErrTokenExpired
	}
	result, err := tx.ExecContext(ctx, `UPDATE tokens SET used_at = ? WHERE hash = ? AND used_at IS NULL AND revoked_at IS NULL`, now, hash)
	if err != nil {
		return TokenPair{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return TokenPair{}, ErrInvalidToken // 同时有另一次刷新先用掉了它
	}
	account, err := scanAccount(tx.QueryRowContext(ctx, selectAccount+` WHERE id = ?`, accountID))
	if err != nil {
		return TokenPair{}, err
	}
	if err := usable(account); err != nil {
		return TokenPair{}, err
	}
	pair, err := s.insertPair(ctx, tx, accountID, family)
	if err != nil {
		return TokenPair{}, err
	}
	return pair, tx.Commit()
}

// Revoke 作废这张刷新令牌所在的那一串（登出）。令牌本来就无效时什么都不做。
func (s *Store) Revoke(ctx context.Context, refreshToken string) error {
	var family string
	err := s.db.QueryRowContext(ctx, `SELECT family FROM tokens WHERE hash = ? AND kind = ?`, hashToken(refreshToken), kindRefresh).
		Scan(&family)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.revokeFamily(ctx, family)
}

// PruneTokens 删掉早就过期的令牌（库不无限长大）。
func (s *Store) PruneTokens(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE expires_at < ?`, s.now().Add(-RefreshTokenTTL).Unix())
	return err
}

func (s *Store) revokeFamily(ctx context.Context, family string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE family = ? AND revoked_at IS NULL`, s.now().Unix(), family)
	return err
}

func (s *Store) insertPair(ctx context.Context, tx *sql.Tx, accountID, family string) (TokenPair, error) {
	access, err := newToken()
	if err != nil {
		return TokenPair{}, err
	}
	refresh, err := newToken()
	if err != nil {
		return TokenPair{}, err
	}
	now := s.now()
	for _, token := range []struct {
		value, kind string
		ttl         time.Duration
	}{{access, kindAccess, AccessTokenTTL}, {refresh, kindRefresh, RefreshTokenTTL}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tokens (hash, kind, account_id, family, expires_at) VALUES (?, ?, ?, ?, ?)`,
			hashToken(token.value), token.kind, accountID, family, now.Add(token.ttl).Unix()); err != nil {
			return TokenPair{}, fmt.Errorf("记令牌: %w", err)
		}
	}
	return TokenPair{AccessToken: access, RefreshToken: refresh, ExpiresIn: AccessTokenTTL}, nil
}

// newToken 是随机令牌（256 位，URL 安全的 base64）。
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
