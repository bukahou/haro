// Package conversationstore 存对话（实现 brain.ConversationStore）：SQLite，表结构由 goose 迁移管理。
//
// 对话里是家里人说过的话：库文件权限 0600。保存一个人的对话时在一个事务里整份替换（几轮而已），
// 写到一半出错不会留下半份。时间按 UTC 的 Unix 秒存，显示时才换成本地时间。
package conversationstore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // database/sql 的 sqlite 驱动

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/sqliteschema"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// operationTimeout：读、存一个人的对话最多等这么久（接口不带 context）。
const operationTimeout = 5 * time.Second

// SQLiteStore 把对话存在一个 SQLite 文件里。
type SQLiteStore struct{ db *sql.DB }

var _ brain.ConversationStore = (*SQLiteStore)(nil)

// OpenSQLite 打开（没有就建）对话库，并把表结构升到最新。
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("创建对话库目录: %w", err)
		}
	}
	// 先以 0600 建文件：SQLite 自己建的是 0644；WAL 等附属文件沿用库文件的权限
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开对话库: %w", err)
	}
	file.Close()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // 单写者
	if err := sqliteschema.Migrate(ctx, db, migrationFiles); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级对话库的表结构: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close 关闭对话库。
func (s *SQLiteStore) Close() error { return s.db.Close() }

const (
	partHistory      = "history"
	partUnsummarized = "unsummarized"
)

// LoadConversation 读一个人的对话；没存过时 ok=false。
func (s *SQLiteStore) LoadConversation(person string) (brain.ConversationRecord, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	record := brain.ConversationRecord{Person: person}
	var openTurn sql.NullString
	var updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT persona_id, summary, tool_call_count, open_turn, updated_at FROM conversations WHERE person = ?`, person).
		Scan(&record.PersonaID, &record.Summary, &record.ToolCallCount, &openTurn, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return brain.ConversationRecord{}, false, nil
	}
	if err != nil {
		return brain.ConversationRecord{}, false, err
	}
	record.UpdatedAt = time.Unix(updated, 0)
	if openTurn.Valid {
		var turn brain.ConversationTurn
		if err := json.Unmarshal([]byte(openTurn.String), &turn); err != nil {
			return brain.ConversationRecord{}, false, fmt.Errorf("解析 %s 没收尾的一轮: %w", person, err)
		}
		record.OpenTurn = &turn
	}
	rows, err := s.db.QueryContext(ctx, `SELECT part, at, messages FROM turns WHERE person = ? ORDER BY part, position`, person)
	if err != nil {
		return brain.ConversationRecord{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var part, messages string
		var at sql.NullInt64
		if err := rows.Scan(&part, &at, &messages); err != nil {
			return brain.ConversationRecord{}, false, err
		}
		turn := brain.ConversationTurn{}
		if at.Valid {
			turn.At = time.Unix(at.Int64, 0)
		}
		if err := json.Unmarshal([]byte(messages), &turn.Messages); err != nil {
			return brain.ConversationRecord{}, false, fmt.Errorf("解析 %s 的一轮对话: %w", person, err)
		}
		switch part {
		case partHistory:
			record.History = append(record.History, turn)
		case partUnsummarized:
			record.Unsummarized = append(record.Unsummarized, turn)
		}
	}
	return record, true, rows.Err()
}

// SaveConversation 在一个事务里整份替换一个人的对话。
func (s *SQLiteStore) SaveConversation(record brain.ConversationRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	var openTurn sql.NullString
	if record.OpenTurn != nil {
		data, err := json.Marshal(record.OpenTurn)
		if err != nil {
			return err
		}
		openTurn = sql.NullString{String: string(data), Valid: true}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations (person, persona_id, summary, tool_call_count, open_turn, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (person) DO UPDATE SET persona_id = excluded.persona_id, summary = excluded.summary,
			tool_call_count = excluded.tool_call_count, open_turn = excluded.open_turn, updated_at = excluded.updated_at`,
		record.Person, record.PersonaID, record.Summary, record.ToolCallCount, openTurn, record.UpdatedAt.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM turns WHERE person = ?`, record.Person); err != nil {
		return err
	}
	for _, part := range []struct {
		name  string
		turns []brain.ConversationTurn
	}{{partHistory, record.History}, {partUnsummarized, record.Unsummarized}} {
		for position, turn := range part.turns {
			messages, err := json.Marshal(turn.Messages)
			if err != nil {
				return err
			}
			var at sql.NullInt64
			if !turn.At.IsZero() {
				at = sql.NullInt64{Int64: turn.At.Unix(), Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO turns (person, part, position, at, messages) VALUES (?, ?, ?, ?, ?)`,
				record.Person, part.name, position, at, string(messages)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// has 判断库里有没有这个人的对话。
func (s *SQLiteStore) has(ctx context.Context, person string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM conversations WHERE person = ?`, person).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
