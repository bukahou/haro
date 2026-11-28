package conversationstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// 线程的信息（第 16 段）：App 里新建的对话各是一个线程；语音那条（brain.VoiceThread）没有这一行。

// ErrThreadNotFound 是没有这条线程（不是这个人的、或者没建过）。
var ErrThreadNotFound = errors.New("没有这条对话")

// Thread 是一条 App 里的对话。
type Thread struct {
	Person, ID string
	Title      string // 空 = 还没起标题（第一轮之后后台起）
	Incognito  bool
	CreatedAt  time.Time
	DeletedAt  *time.Time
	UpdatedAt  time.Time // 最近一轮的时间（还没有轮次时是建的时间）
	LastText   string    // 最近一轮 haro 的回答（列表里显示一行）
}

// CreateThread 给这个人建一条新的对话。
func (s *SQLiteStore) CreateThread(ctx context.Context, person, id string, incognito bool, now time.Time) (Thread, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO threads (person, thread, incognito, created_at) VALUES (?, ?, ?, ?)`,
		person, id, incognito, now.Unix()); err != nil {
		return Thread{}, fmt.Errorf("建对话: %w", err)
	}
	return s.ThreadOf(ctx, person, id)
}

// ThreadOf 是这个人的一条对话（删了的也给，DeletedAt 不为空）。
func (s *SQLiteStore) ThreadOf(ctx context.Context, person, id string) (Thread, error) {
	threads, err := s.threads(ctx, ` WHERE h.person = ? AND h.thread = ?`, person, id)
	if err != nil {
		return Thread{}, err
	}
	if len(threads) == 0 {
		return Thread{}, ErrThreadNotFound
	}
	return threads[0], nil
}

// Threads 是这个人的对话（新的在前）；withDeleted 为 false 时不含删了的。
func (s *SQLiteStore) Threads(ctx context.Context, person string, withDeleted bool) ([]Thread, error) {
	where := ` WHERE h.person = ?`
	if !withDeleted {
		where += ` AND h.deleted_at IS NULL`
	}
	return s.threads(ctx, where+` ORDER BY updated DESC, h.created_at DESC`, person)
}

func (s *SQLiteStore) threads(ctx context.Context, where string, args ...any) ([]Thread, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.person, h.thread, h.title, h.incognito, h.created_at, h.deleted_at,
		COALESCE((SELECT MAX(t.at) FROM transcript t WHERE t.person = h.person AND t.thread = h.thread), h.created_at) AS updated,
		(SELECT t.messages FROM transcript t WHERE t.person = h.person AND t.thread = h.thread ORDER BY t.seq DESC LIMIT 1)
		FROM threads h`+where, args...)
	if err != nil {
		return nil, fmt.Errorf("读对话: %w", err)
	}
	defer rows.Close()
	var threads []Thread
	for rows.Next() {
		var thread Thread
		var created, updated int64
		var deleted sql.NullInt64
		var last sql.NullString
		if err := rows.Scan(&thread.Person, &thread.ID, &thread.Title, &thread.Incognito, &created, &deleted, &updated, &last); err != nil {
			return nil, fmt.Errorf("读对话: %w", err)
		}
		thread.CreatedAt, thread.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
		if deleted.Valid {
			at := time.Unix(deleted.Int64, 0).UTC()
			thread.DeletedAt = &at
		}
		if last.Valid {
			thread.LastText = lastAnswer(last.String)
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// lastAnswer 是一轮里 haro 最后的回答（解不开时为空：列表里只是少一行预览）。
func lastAnswer(data string) string {
	var messages []brain.ChatMessage
	if json.Unmarshal([]byte(data), &messages) != nil {
		return ""
	}
	answer := ""
	for _, message := range messages {
		if message.Role == brain.RoleAssistant && message.Text != "" {
			answer = message.Text
		}
	}
	return answer
}

// SetThreadTitle 给一条对话起标题。
func (s *SQLiteStore) SetThreadTitle(ctx context.Context, person, id, title string) error {
	return s.updateThread(ctx, `UPDATE threads SET title = ? WHERE person = ? AND thread = ?`, title, person, id)
}

// DeleteThread 删一条对话（软删除：能恢复）。
func (s *SQLiteStore) DeleteThread(ctx context.Context, person, id string, now time.Time) error {
	return s.updateThread(ctx, `UPDATE threads SET deleted_at = ? WHERE person = ? AND thread = ? AND deleted_at IS NULL`,
		now.Unix(), person, id)
}

// RestoreThread 恢复删了的对话。
func (s *SQLiteStore) RestoreThread(ctx context.Context, person, id string) error {
	return s.updateThread(ctx, `UPDATE threads SET deleted_at = NULL WHERE person = ? AND thread = ? AND deleted_at IS NOT NULL`, person, id)
}

func (s *SQLiteStore) updateThread(ctx context.Context, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("改对话: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return ErrThreadNotFound
	}
	return nil
}

// RecordedTurn 是完整记录里的一轮（往前翻消息用）：编号、时间、这一轮的消息原样。
type RecordedTurn struct {
	Seq      int64
	At       time.Time // 零值 = 不知道
	Messages []brain.ChatMessage
}

// ThreadTurns 往前翻一条对话的完整记录：编号小于 before 的最近 limit 轮（before 为 0 = 从最新的开始），旧的在前。
func (s *SQLiteStore) ThreadTurns(ctx context.Context, person, thread string, before int64, limit int) ([]RecordedTurn, error) {
	query := `SELECT seq, at, messages FROM transcript WHERE person = ? AND thread = ?`
	args := []any{person, thread}
	if before > 0 {
		query += ` AND seq < ?`
		args = append(args, before)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY seq DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("读对话记录: %w", err)
	}
	defer rows.Close()
	var turns []RecordedTurn
	for rows.Next() {
		var turn RecordedTurn
		var at sql.NullInt64
		var data string
		if err := rows.Scan(&turn.Seq, &at, &data); err != nil {
			return nil, fmt.Errorf("读对话记录: %w", err)
		}
		if at.Valid {
			turn.At = time.Unix(at.Int64, 0).UTC()
		}
		if err := json.Unmarshal([]byte(data), &turn.Messages); err != nil {
			return nil, fmt.Errorf("对话 %s #%d 的消息解不开: %w", person, turn.Seq, err)
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns, nil
}
