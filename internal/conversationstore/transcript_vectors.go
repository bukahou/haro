package conversationstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/embedding"
)

// 完整对话记录的向量（v10 D5）：以前的对话也能按意思找（「上个月聊过的那家店」）。向量按轮存在 transcript_vectors；
// 这一轮的文字后来变了（被打断、改成主人听到的部分）时，存下的摘要对不上就重算。

// TranscriptTurn 是完整记录里的一轮：主人说的话和 haro 最后的回答（工具往来不算）。
type TranscriptTurn struct {
	Person   string
	Seq      int64
	At       time.Time // 零值 = 不知道
	Said     string    // 主人说的（这一轮没有主人的话时为空，例如 haro 到点自己开口的提醒）
	Answered string    // haro 最后说的
}

// Text 是拿去算向量的文字：主人说的 + haro 答的（各一行）。
func (t TranscriptTurn) Text() string {
	var parts []string
	if t.Said != "" {
		parts = append(parts, "主人："+t.Said)
	}
	if t.Answered != "" {
		parts = append(parts, "haro："+t.Answered)
	}
	return strings.Join(parts, "\n")
}

// Digest 是这一轮文字的摘要（sha256，十六进制）：存向量时一并存下，之后文字变了就对不上。
func (t TranscriptTurn) Digest() string {
	sum := sha256.Sum256([]byte(t.Text()))
	return hex.EncodeToString(sum[:])
}

// TurnVector 是一轮的向量。
type TurnVector struct {
	Person string
	Seq    int64
	Vector []float32
}

// recheckRecentTurns：每次找要算向量的轮次时，最近这么多轮即使有向量也重新核对一遍摘要。文字只在刚说完的那一轮被改
// （播放中被打断时改成听到的部分），更早的不会再变，不用每次全表核对；定值
const recheckRecentTurns = 50

// notIncognito 是「不在无痕线程里」的条件（t 是 transcript）：无痕线程的轮次不算向量，长期记忆按意思找不到它们。
const notIncognito = `NOT EXISTS (SELECT 1 FROM threads h WHERE h.person = t.person AND h.thread = t.thread AND h.incognito = 1)`

// UnindexedTurns 返回要算向量的轮次（旧的在前），最多 limit 轮：还没有这个模型的向量的，加上最近几轮里文字已经变了的。
// 无痕线程的不算。
func (s *SQLiteStore) UnindexedTurns(ctx context.Context, model string, limit int) ([]TranscriptTurn, error) {
	missing, err := s.turns(ctx, `SELECT t.person, t.seq, t.at, t.messages FROM transcript t
		LEFT JOIN transcript_vectors v ON v.person = t.person AND v.seq = t.seq AND v.model = ?
		WHERE v.seq IS NULL AND `+notIncognito+` ORDER BY t.at, t.person, t.seq LIMIT ?`, model, limit)
	if err != nil || len(missing) >= limit {
		return missing, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.person, t.seq, t.at, t.messages, v.digest FROM transcript t
		JOIN transcript_vectors v ON v.person = t.person AND v.seq = t.seq AND v.model = ?
		ORDER BY t.at DESC, t.seq DESC LIMIT ?`, model, recheckRecentTurns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() && len(missing) < limit {
		var person, messages, digest string
		var seq int64
		var at sql.NullInt64
		if err := rows.Scan(&person, &seq, &at, &messages, &digest); err != nil {
			return nil, err
		}
		turn, err := transcriptTurn(person, seq, at, messages)
		if err != nil {
			return nil, err
		}
		if turn.Digest() != digest {
			missing = append(missing, turn)
		}
	}
	return missing, rows.Err()
}

// SaveTurnVector 存一轮的向量（同一轮再存就替换）。
func (s *SQLiteStore) SaveTurnVector(ctx context.Context, person string, seq int64, model, digest string, vector []float32) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO transcript_vectors (person, seq, model, digest, vector) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (person, seq) DO UPDATE SET model = excluded.model, digest = excluded.digest, vector = excluded.vector`,
		person, seq, model, digest, embedding.EncodeVector(vector))
	return err
}

// TurnVectors 返回这个模型的全部轮次向量。
func (s *SQLiteStore) TurnVectors(ctx context.Context, model string) ([]TurnVector, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT person, seq, vector FROM transcript_vectors WHERE model = ? ORDER BY person, seq`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vectors []TurnVector
	for rows.Next() {
		var item TurnVector
		var data []byte
		if err := rows.Scan(&item.Person, &item.Seq, &data); err != nil {
			return nil, err
		}
		if item.Vector, err = embedding.DecodeVector(data); err != nil {
			return nil, fmt.Errorf("对话 %s #%d 的向量: %w", item.Person, item.Seq, err)
		}
		vectors = append(vectors, item)
	}
	return vectors, rows.Err()
}

// TranscriptTurns 按轮次编号取这个人的几轮（按 seqs 的顺序；没有的不出现）。
func (s *SQLiteStore) TranscriptTurns(ctx context.Context, person string, seqs []int64) ([]TranscriptTurn, error) {
	var turns []TranscriptTurn
	for _, seq := range seqs {
		found, err := s.turns(ctx, `SELECT person, seq, at, messages FROM transcript WHERE person = ? AND seq = ?`, person, seq)
		if err != nil {
			return nil, err
		}
		turns = append(turns, found...)
	}
	return turns, nil
}

func (s *SQLiteStore) turns(ctx context.Context, query string, args ...any) ([]TranscriptTurn, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []TranscriptTurn
	for rows.Next() {
		var person, messages string
		var seq int64
		var at sql.NullInt64
		if err := rows.Scan(&person, &seq, &at, &messages); err != nil {
			return nil, err
		}
		turn, err := transcriptTurn(person, seq, at, messages)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

// transcriptTurn 从存下的消息里取出主人说的话（不是系统附注的那几条用户消息）和 haro 最后的回答。
func transcriptTurn(person string, seq int64, at sql.NullInt64, data string) (TranscriptTurn, error) {
	var messages []brain.ChatMessage
	if err := json.Unmarshal([]byte(data), &messages); err != nil {
		return TranscriptTurn{}, fmt.Errorf("对话 %s #%d 的消息解不开: %w", person, seq, err)
	}
	turn := TranscriptTurn{Person: person, Seq: seq}
	if at.Valid {
		turn.At = time.Unix(at.Int64, 0).UTC()
	}
	var said []string
	for _, message := range messages {
		switch {
		case message.Role == brain.RoleUser && !message.Synthetic && message.Text != "":
			said = append(said, message.Text)
		case message.Role == brain.RoleAssistant && message.Text != "":
			turn.Answered = message.Text // 最后一条有文字的就是最后的回答
		}
	}
	turn.Said = strings.Join(said, " ")
	return turn, nil
}
