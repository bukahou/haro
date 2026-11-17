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

// SQLiteStore 是 Repository 的 SQLite 实现（开发阶段）。
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

// searchWindow 是查找时最多拿出来打分的记录数（按时间倒序）。个人记事本远到不了这个量。
const searchWindow = 2000

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

// Recent 返回最近 limit 条（未删除的）。
func (s *SQLiteStore) Recent(ctx context.Context, limit int) ([]Note, error) {
	return s.query(ctx, `SELECT id, content, created_at FROM notes WHERE deleted_at IS NULL
		ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
}

// Search 取最近 searchWindow 条，交给 RankByRelevance 打分。
func (s *SQLiteStore) Search(ctx context.Context, query string, limit int) ([]Note, error) {
	candidates, err := s.Recent(ctx, searchWindow)
	if err != nil {
		return nil, err
	}
	return RankByRelevance(candidates, query, limit), nil
}

// Delete 软删除一条：只打标记，数据还在，需要时可以找回。
func (s *SQLiteStore) Delete(ctx context.Context, id int64) (Note, error) {
	notes, err := s.query(ctx, `SELECT id, content, created_at FROM notes WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		return Note{}, err
	}
	if len(notes) == 0 {
		return Note{}, ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `UPDATE notes SET deleted_at = ? WHERE id = ?`, time.Now().Format(time.RFC3339), id)
	return notes[0], err
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
		if err := rows.Scan(&note.ID, &note.Content, &created); err != nil {
			return nil, err
		}
		note.CreatedAt, _ = time.Parse(time.RFC3339, created)
		notes = append(notes, note)
	}
	return notes, rows.Err()
}
