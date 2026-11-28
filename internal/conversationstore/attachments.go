package conversationstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// ErrAttachmentNotFound 是没有这张图（不是这个人的、或者没传过）。
var ErrAttachmentNotFound = errors.New("没有这张图片")

// Attachment 是 App 里发的一张图片（图片本身在 internal/attachments 的文件里）。
type Attachment struct {
	ID, Person, Thread string
	Width, Height      int
	Bytes              int
	CreatedAt          time.Time
}

// AddAttachment 记下一张图片。
func (s *SQLiteStore) AddAttachment(ctx context.Context, attachment Attachment) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO attachments (id, person, thread, width, height, bytes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		attachment.ID, attachment.Person, attachment.Thread, attachment.Width, attachment.Height, attachment.Bytes, attachment.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("记图片: %w", err)
	}
	return nil
}

// AttachmentOf 是这个人的一张图片。
func (s *SQLiteStore) AttachmentOf(ctx context.Context, person, id string) (Attachment, error) {
	attachment := Attachment{ID: id, Person: person}
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT thread, width, height, bytes, created_at FROM attachments WHERE id = ? AND person = ?`, id, person).
		Scan(&attachment.Thread, &attachment.Width, &attachment.Height, &attachment.Bytes, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrAttachmentNotFound
	} else if err != nil {
		return Attachment{}, fmt.Errorf("读图片: %w", err)
	}
	attachment.CreatedAt = time.Unix(created, 0).UTC()
	return attachment, nil
}

// FoundTurn 是搜到的一轮：在哪条对话、第几轮、这一轮的消息。
type FoundTurn struct {
	Thread string
	RecordedTurn
}

// notDeletedThread 是「不在删了的对话里」的条件（t 是 transcript）：语音那条没有 threads 的行，照算。
const notDeletedThread = `NOT EXISTS (SELECT 1 FROM threads h WHERE h.person = t.person AND h.thread = t.thread AND h.deleted_at IS NOT NULL)`

// SearchTurns 按文字找这个人以前的对话：主人说的或 haro 答的里含着 query（子串匹配，不分大小写，不按语言分词），
// 新的在前，最多 limit 轮；删了的对话里的不算。系统附注、工具的结果不算。
func (s *SQLiteStore) SearchTurns(ctx context.Context, person, query string, limit int) ([]FoundTurn, error) {
	pattern := "%" + escapeLike(query) + "%"
	return s.foundTurns(ctx, `SELECT t.thread, t.seq, t.at, t.messages FROM transcript t
		WHERE t.person = ? AND `+notDeletedThread+` AND EXISTS (SELECT 1 FROM json_each(t.messages) m
			WHERE json_extract(m.value, '$.role') IN ('user', 'assistant') AND COALESCE(json_extract(m.value, '$.synthetic'), 0) = 0
			AND json_extract(m.value, '$.text') LIKE ? ESCAPE '\')
		ORDER BY t.seq DESC LIMIT ?`, person, pattern, limit)
}

// TurnsBySeq 是这个人编号为 seqs 的几轮（带在哪条对话；删了的对话里的不给）。
func (s *SQLiteStore) TurnsBySeq(ctx context.Context, person string, seqs []int64) ([]FoundTurn, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	args := []any{person}
	for _, seq := range seqs {
		args = append(args, seq)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(seqs)), ", ")
	return s.foundTurns(ctx, `SELECT t.thread, t.seq, t.at, t.messages FROM transcript t
		WHERE t.person = ? AND `+notDeletedThread+` AND t.seq IN (`+placeholders+`) ORDER BY t.seq DESC`, args...)
}

func (s *SQLiteStore) foundTurns(ctx context.Context, query string, args ...any) ([]FoundTurn, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("找对话: %w", err)
	}
	defer rows.Close()
	var found []FoundTurn
	for rows.Next() {
		var turn FoundTurn
		var at sql.NullInt64
		var data string
		if err := rows.Scan(&turn.Thread, &turn.Seq, &at, &data); err != nil {
			return nil, fmt.Errorf("找对话: %w", err)
		}
		if at.Valid {
			turn.At = time.Unix(at.Int64, 0).UTC()
		}
		if turn.Messages, err = decodeMessages(data); err != nil {
			return nil, fmt.Errorf("第 %d 轮的消息解不开: %w", turn.Seq, err)
		}
		found = append(found, turn)
	}
	return found, rows.Err()
}

// escapeLike 把 LIKE 的通配符（% _）和转义符本身转义掉：主人搜的字照字面找。
func escapeLike(text string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
}

// decodeMessages 解一轮存下的消息（JSON）。
func decodeMessages(data string) ([]brain.ChatMessage, error) {
	var messages []brain.ChatMessage
	if err := json.Unmarshal([]byte(data), &messages); err != nil {
		return nil, err
	}
	return messages, nil
}
