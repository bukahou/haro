package notes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 的 SQLite 驱动：编译进程序，不用装任何软件、不用起数据库服务
)

// SQLiteStore 是 Repository 的 SQLite 实现。
type SQLiteStore struct {
	db *sql.DB
}

var _ Repository = (*SQLiteStore)(nil)

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS notes (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	content    TEXT    NOT NULL,
	created_at TEXT    NOT NULL,
	deleted_at TEXT
);
CREATE INDEX IF NOT EXISTS notes_created_at ON notes (created_at);`

// OpenSQLite 打开（必要时创建目录、文件与表）SQLite 数据库。
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建记事本目录: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者；记事本的并发量用不着更多
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化记事本数据库 %s: %w", path, err)
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
	now := time.Now()
	result, err := s.db.ExecContext(ctx, `INSERT INTO notes (content, created_at) VALUES (?, ?)`,
		content, now.Format(time.RFC3339))
	if err != nil {
		return Note{}, err
	}
	id, err := result.LastInsertId()
	return Note{ID: id, Content: content, CreatedAt: now}, err
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
	return s.mark(ctx, id, `deleted_at IS NULL`, sql.NullString{String: time.Now().Format(time.RFC3339), Valid: true})
}

// Restore 恢复一条删掉的。
func (s *SQLiteStore) Restore(ctx context.Context, id int64) (Note, error) {
	return s.mark(ctx, id, `deleted_at IS NOT NULL`, sql.NullString{})
}

// mark 把满足 state 的那一条的 deleted_at 改成 deletedAt；没有这样的一条时返回 ErrNotFound。
func (s *SQLiteStore) mark(ctx context.Context, id int64, state string, deletedAt sql.NullString) (Note, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE notes SET deleted_at = ? WHERE id = ? AND `+state, deletedAt, id)
	if err != nil {
		return Note{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return Note{}, errors.Join(ErrNotFound, err)
	}
	notes, err := s.query(ctx, `SELECT id, content, created_at, deleted_at FROM notes WHERE id = ?`, id)
	if err != nil || len(notes) == 0 {
		return Note{}, errors.Join(ErrNotFound, err)
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
		var created string
		var deleted sql.NullString
		if err := rows.Scan(&note.ID, &note.Content, &created, &deleted); err != nil {
			return nil, err
		}
		if note.CreatedAt, err = time.Parse(time.RFC3339, created); err != nil {
			return nil, fmt.Errorf("记录 %d 的时间格式不对: %w", note.ID, err)
		}
		if deleted.Valid {
			if note.DeletedAt, err = time.Parse(time.RFC3339, deleted.String); err != nil {
				return nil, fmt.Errorf("记录 %d 的删除时间格式不对: %w", note.ID, err)
			}
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}
