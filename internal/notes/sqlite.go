package notes

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/bukahou/haro/internal/sqliteschema"
)

// SQLiteStore 是 Repository 的 SQLite 实现。时间按 UTC 的 Unix 秒存（迁移 2），给人看时才换成本地时间。
type SQLiteStore struct {
	db *sql.DB
}

var _ Repository = (*SQLiteStore)(nil)

// 记事库的表结构由迁移文件管理（sqliteschema）；迁移 2 要逐条转换时间，用 Go 写（notes_migrations.go）。
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// OpenSQLite 打开（必要时创建目录和文件）SQLite 数据库，并把表结构升到最新。
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	db, err := sqliteschema.Open(ctx, path, migrationFiles,
		sqliteschema.Options{Migrate: []goose.ProviderOption{goose.WithGoMigrations(unixTimeMigration)}})
	if err != nil {
		return nil, fmt.Errorf("打开记事本数据库 %s: %w", path, err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close 关闭数据库。
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Save 记一条。
func (s *SQLiteStore) Save(ctx context.Context, content string) (Note, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Note{}, errors.New("内容为空")
	}
	created := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `INSERT INTO notes (content, created_at) VALUES (?, ?)`, content, created)
	if err != nil {
		return Note{}, err
	}
	id, err := result.LastInsertId()
	return Note{ID: id, Content: content, CreatedAt: time.Unix(created, 0).UTC()}, err
}

// List 返回最近 limit 条（没删掉的，新的在前）和一共有几条。
func (s *SQLiteStore) List(ctx context.Context, limit int) ([]Note, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes WHERE deleted_at IS NULL`).Scan(&total); err != nil {
		return nil, 0, err
	}
	notes, err := s.query(ctx, `SELECT id, content, created_at, deleted_at FROM notes WHERE deleted_at IS NULL
		ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	return notes, total, err
}

// Deleted 返回最近删掉的 limit 条。
func (s *SQLiteStore) Deleted(ctx context.Context, limit int) ([]Note, error) {
	return s.query(ctx, `SELECT id, content, created_at, deleted_at FROM notes WHERE deleted_at IS NOT NULL
		ORDER BY deleted_at DESC, id DESC LIMIT ?`, limit)
}

// Delete 软删除一条：只打标记，数据还在，可以恢复。
func (s *SQLiteStore) Delete(ctx context.Context, id int64) (Note, error) {
	return s.mark(ctx, id, `deleted_at IS NULL`, sql.NullInt64{Int64: time.Now().Unix(), Valid: true})
}

// Restore 恢复一条删掉的。
func (s *SQLiteStore) Restore(ctx context.Context, id int64) (Note, error) {
	return s.mark(ctx, id, `deleted_at IS NOT NULL`, sql.NullInt64{})
}

// mark 把满足 state 的那一条的 deleted_at 改成 deletedAt，交回改完的那一条；没有这样的一条时返回 ErrNotFound。
// 改和读回是同一条语句（RETURNING）：出错就是出错（交回原来的错误），不会把「已经改了、读回时出错」说成「没有这一条」。
func (s *SQLiteStore) mark(ctx context.Context, id int64, state string, deletedAt sql.NullInt64) (Note, error) {
	notes, err := s.query(ctx, `UPDATE notes SET deleted_at = ? WHERE id = ? AND `+state+
		` RETURNING id, content, created_at, deleted_at`, deletedAt, id)
	switch {
	case err != nil:
		return Note{}, err
	case len(notes) == 0:
		return Note{}, ErrNotFound
	}
	return notes[0], nil
}

func (s *SQLiteStore) query(ctx context.Context, sqlText string, args ...any) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []Note
	for rows.Next() {
		var note Note
		var created int64
		var deleted sql.NullInt64
		if err := rows.Scan(&note.ID, &note.Content, &created, &deleted); err != nil {
			return nil, err
		}
		note.CreatedAt = time.Unix(created, 0).UTC()
		if deleted.Valid {
			note.DeletedAt = time.Unix(deleted.Int64, 0).UTC()
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}
