// Package conversationstore 存对话（实现 brain.ConversationStore）：SQLite，表结构由 goose 迁移管理。
//
// 对话里是家里人说过的话：库文件权限 0600。保存一个人的对话时在一个事务里整份替换（几轮而已），
// 写到一半出错不会留下半份；同一个事务里把这几轮写进完整的对话记录（transcript：按轮次编号，只增不删）。
// 时间按 UTC 的 Unix 秒存，显示时才换成本地时间。
package conversationstore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// OpenSQLite 打开（没有就建）对话库，并把表结构升到最新。库里是家里人说过的话：目录 0700、文件 0600。
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	db, err := sqliteschema.Open(ctx, path, migrationFiles, sqliteschema.Options{Private: true, ForeignKeys: true})
	if err != nil {
		return nil, fmt.Errorf("打开对话库: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close 关闭对话库。
func (s *SQLiteStore) Close() error { return s.db.Close() }

const (
	partHistory      = "history"
	partUnsummarized = "unsummarized"
)

// LoadConversation 读一个人的一条对话；没存过时 ok=false。
func (s *SQLiteStore) LoadConversation(person, thread string) (brain.ConversationRecord, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	record := brain.ConversationRecord{Person: person, Thread: thread}
	var openTurn sql.NullString
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT persona_id, summary, tool_call_count, turn_count, open_turn, updated_at FROM conversations
		WHERE person = ? AND thread = ?`, person, thread).
		Scan(&record.PersonaID, &record.Summary, &record.ToolCallCount, &record.TurnCount, &openTurn, &updated)
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
	rows, err := s.db.QueryContext(ctx, `SELECT part, seq, at, messages FROM turns WHERE person = ? AND thread = ? ORDER BY part, position`,
		person, thread)
	if err != nil {
		return brain.ConversationRecord{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var part, messages string
		var seq, at sql.NullInt64
		if err := rows.Scan(&part, &seq, &at, &messages); err != nil {
			return brain.ConversationRecord{}, false, err
		}
		turn := brain.ConversationTurn{Seq: int(seq.Int64)} // NULL（迁移之前没编号的）= 0：大脑读出来时补编号
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

// SaveConversation 在一个事务里整份替换一个人的对话，并把其中编了号的轮次写进完整的对话记录。
// 完整记录只增不删：同一编号已经有一轮时，只有说话的时间相同（同一轮：被打断后改成主人听到的部分）才更新；
// 时间不同就是另一轮，不覆盖它（这份对话照样存下），提交之后返回说明哪几轮没写进记录的错误。
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations (person, thread, persona_id, summary, tool_call_count, turn_count, open_turn,
		updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (person, thread) DO UPDATE SET persona_id = excluded.persona_id, summary = excluded.summary,
			tool_call_count = excluded.tool_call_count, turn_count = excluded.turn_count, open_turn = excluded.open_turn,
			updated_at = excluded.updated_at`,
		record.Person, record.Thread, record.PersonaID, record.Summary, record.ToolCallCount, record.TurnCount, openTurn,
		record.UpdatedAt.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM turns WHERE person = ? AND thread = ?`, record.Person, record.Thread); err != nil {
		return err
	}
	var kept []int64 // 完整记录里已经是另一轮、没有覆盖的编号
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
			var seq sql.NullInt64
			if turn.Seq > 0 {
				seq = sql.NullInt64{Int64: int64(turn.Seq), Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO turns (person, thread, part, position, seq, at, messages) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				record.Person, record.Thread, part.name, position, seq, at, string(messages)); err != nil {
				return err
			}
			if !seq.Valid { // 没编号的不进完整记录（大脑读出来时会补编号，下次保存再写）
				continue
			}
			// 完整记录：只增不删；同一轮再写一次（被打断、改成主人听到的部分）时跟着更新。
			// 同一轮 = 说话的时间相同（IS：旧记录里都不知道时间的也算同一轮）；不同时改 0 行
			// 同一个人的编号所有线程共用：同一编号、同一时间、同一线程才算同一轮
			result, err := tx.ExecContext(ctx, `INSERT INTO transcript (person, seq, thread, at, messages) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (person, seq) DO UPDATE SET messages = excluded.messages
				WHERE transcript.at IS excluded.at AND transcript.thread = excluded.thread`,
				record.Person, seq, record.Thread, at, string(messages))
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed == 0 {
				kept = append(kept, seq.Int64)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(kept) > 0 {
		return &TranscriptKeptError{Person: record.Person, Seqs: kept}
	}
	return nil
}

// LastSeq 是这个人用过的最大轮次编号（所有线程、完整记录里的都算；没有时为 0）。
func (s *SQLiteStore) LastSeq(person string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	var last int
	err := s.db.QueryRowContext(ctx, `SELECT MAX(COALESCE((SELECT MAX(seq) FROM transcript WHERE person = ?), 0),
		COALESCE((SELECT MAX(turn_count) FROM conversations WHERE person = ?), 0))`, person, person).Scan(&last)
	return last, err
}

// TranscriptKeptError 表示保存时有几轮没写进完整的对话记录：记录里同一编号已经是另一轮（说话的时间不同），没有覆盖它。
// 这份对话本身照样存下了。
type TranscriptKeptError struct {
	Person string
	Seqs   []int64
}

func (e *TranscriptKeptError) Error() string {
	return fmt.Sprintf("完整的对话记录里 %s 的第 %v 轮已经是另一轮（说话的时间不同），没有覆盖；这份对话照样存下了", e.Person, e.Seqs)
}

// ErasePerson 删掉这个人的全部对话（各条线程、完整记录和向量、背景记录池、附件的记录），返回附件的编号（文件由调用方删）。彻底删账号时用。
func (s *SQLiteStore) ErasePerson(ctx context.Context, person string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM attachments WHERE person = ?`, person)
	if err != nil {
		return nil, fmt.Errorf("读附件: %w", err)
	}
	var attachmentIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		attachmentIDs = append(attachmentIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	err = sqliteschema.ErasePerson(ctx, s.db, person,
		`DELETE FROM transcript_vectors WHERE person = ?`, `DELETE FROM transcript WHERE person = ?`,
		`DELETE FROM turns WHERE person = ?`, `DELETE FROM conversations WHERE person = ?`, `DELETE FROM threads WHERE person = ?`,
		`DELETE FROM attachments WHERE person = ?`, `DELETE FROM backstory_items WHERE person = ?`, `DELETE FROM backstory_state WHERE person = ?`)
	return attachmentIDs, err
}
