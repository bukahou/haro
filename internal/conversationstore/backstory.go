package conversationstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrBackstoryItemNotFound 是没有这一条（不是这个人的、或者没有）。
var ErrBackstoryItemNotFound = errors.New("背景记录池里没有这一条")

// BackstoryItem 是背景记录池里的一条。
type BackstoryItem struct {
	ID           string
	Text         string
	SourceThread string // 从哪条对话的哪一轮来；SourceSeq 为 0 = 不知道
	SourceSeq    int64
	Edited       bool // 主人在 App 里改过：后台不改、不删
	UpdatedAt    time.Time
}

// Backstory 是一个人的背景记录池。
type Backstory struct {
	Items          []BackstoryItem
	MergedSeq      int64      // 并到这个人的第几轮了
	Enabled        bool       // 没设过时是开着的
	ConsolidatedAt *time.Time // 最近一次整理；nil = 还没整理过
}

// BackstoryOf 是这个人的背景记录池（没有时是空的、开着的）。
func (s *SQLiteStore) BackstoryOf(ctx context.Context, person string) (Backstory, error) {
	backstory := Backstory{Enabled: true}
	var enabled bool
	var consolidated sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT merged_seq, enabled, consolidated_at FROM backstory_state WHERE person = ?`, person).
		Scan(&backstory.MergedSeq, &enabled, &consolidated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Backstory{}, fmt.Errorf("读背景记录池: %w", err)
	default:
		backstory.Enabled = enabled
		if consolidated.Valid {
			at := time.Unix(consolidated.Int64, 0).UTC()
			backstory.ConsolidatedAt = &at
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, text, source_thread, source_seq, edited, updated_at FROM backstory_items
		WHERE person = ? ORDER BY position`, person)
	if err != nil {
		return Backstory{}, fmt.Errorf("读背景记录池: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item BackstoryItem
		var thread sql.NullString
		var seq sql.NullInt64
		var updated int64
		if err := rows.Scan(&item.ID, &item.Text, &thread, &seq, &item.Edited, &updated); err != nil {
			return Backstory{}, fmt.Errorf("读背景记录池: %w", err)
		}
		item.SourceThread, item.SourceSeq, item.UpdatedAt = thread.String, seq.Int64, time.Unix(updated, 0).UTC()
		backstory.Items = append(backstory.Items, item)
	}
	return backstory, rows.Err()
}

// BackstoryPeople 是有背景记录池的人（启动时读进内存用）。
func (s *SQLiteStore) BackstoryPeople(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT person FROM backstory_state UNION SELECT person FROM backstory_items`)
	if err != nil {
		return nil, fmt.Errorf("读背景记录池: %w", err)
	}
	defer rows.Close()
	var people []string
	for rows.Next() {
		var person string
		if err := rows.Scan(&person); err != nil {
			return nil, err
		}
		people = append(people, person)
	}
	return people, rows.Err()
}

// ReplaceGenerated 换掉这个人后台生成的条目（主人改过的原样留着、排在前面），记下并到第几轮、整理的时间（consolidated 为 nil 时不改）。
func (s *SQLiteStore) ReplaceGenerated(ctx context.Context, person string, generated []BackstoryItem, mergedSeq int64,
	consolidated *time.Time, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM backstory_items WHERE person = ? AND edited = 0`, person); err != nil {
		return fmt.Errorf("换背景记录池: %w", err)
	}
	var position int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM backstory_items WHERE person = ?`, person).
		Scan(&position); err != nil {
		return fmt.Errorf("换背景记录池: %w", err)
	}
	for i, item := range generated {
		var thread, seq any
		if item.SourceSeq > 0 {
			thread, seq = item.SourceThread, item.SourceSeq
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO backstory_items (id, person, text, source_thread, source_seq, edited, position, updated_at)
			VALUES (?, ?, ?, ?, ?, 0, ?, ?)`, item.ID, person, item.Text, thread, seq, position+i, now.Unix()); err != nil {
			return fmt.Errorf("换背景记录池: %w", err)
		}
	}
	var consolidatedAt any
	if consolidated != nil {
		consolidatedAt = consolidated.Unix()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO backstory_state (person, merged_seq, consolidated_at) VALUES (?, ?, ?)
		ON CONFLICT (person) DO UPDATE SET merged_seq = excluded.merged_seq,
			consolidated_at = COALESCE(excluded.consolidated_at, backstory_state.consolidated_at)`,
		person, mergedSeq, consolidatedAt); err != nil {
		return fmt.Errorf("记背景记录池并到哪了: %w", err)
	}
	return tx.Commit()
}

// EditBackstoryItem 是主人改一条（之后后台不改、不删它）。
func (s *SQLiteStore) EditBackstoryItem(ctx context.Context, person, id, text string, now time.Time) error {
	return s.changeBackstoryItem(ctx, `UPDATE backstory_items SET text = ?, edited = 1, updated_at = ? WHERE id = ? AND person = ?`,
		text, now.Unix(), id, person)
}

// DeleteBackstoryItem 是主人删一条。
func (s *SQLiteStore) DeleteBackstoryItem(ctx context.Context, person, id string) error {
	return s.changeBackstoryItem(ctx, `DELETE FROM backstory_items WHERE id = ? AND person = ?`, id, person)
}

func (s *SQLiteStore) changeBackstoryItem(ctx context.Context, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("改背景记录池: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return ErrBackstoryItemNotFound
	}
	return nil
}

// SetBackstoryEnabled 是主人在 App 里开、关背景记录池。
func (s *SQLiteStore) SetBackstoryEnabled(ctx context.Context, person string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO backstory_state (person, enabled) VALUES (?, ?)
		ON CONFLICT (person) DO UPDATE SET enabled = excluded.enabled`, person, enabled)
	return err
}

// PendingPerson 是有新轮次、闲下来了的一个人：最新一轮的编号。
type PendingPerson struct {
	Person  string
	LastSeq int64
}

// PeopleToMerge 是有还没并进背景记录池的轮次、而且最近一轮早于 idleBefore（闲下来了）的人；关了背景记录池的不算。
// 无痕、删了的对话里的轮次不算。
func (s *SQLiteStore) PeopleToMerge(ctx context.Context, idleBefore time.Time) ([]PendingPerson, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.person, MAX(t.seq), MAX(t.at) FROM transcript t
		LEFT JOIN backstory_state b ON b.person = t.person
		WHERE `+notIncognito+` AND `+notDeletedThread+` AND COALESCE(b.enabled, 1) = 1 AND t.seq > COALESCE(b.merged_seq, 0)
		GROUP BY t.person`)
	if err != nil {
		return nil, fmt.Errorf("找要并的人: %w", err)
	}
	defer rows.Close()
	var people []PendingPerson
	for rows.Next() {
		var person PendingPerson
		var last sql.NullInt64
		if err := rows.Scan(&person.Person, &person.LastSeq, &last); err != nil {
			return nil, err
		}
		if last.Valid && time.Unix(last.Int64, 0).Before(idleBefore) {
			people = append(people, person)
		}
	}
	return people, rows.Err()
}

// TurnsAfter 是这个人编号大于 after 的轮次（旧的在前，最多 limit 轮）；无痕、删了的对话里的不算。
func (s *SQLiteStore) TurnsAfter(ctx context.Context, person string, after int64, limit int) ([]FoundTurn, error) {
	return s.foundTurns(ctx, `SELECT t.thread, t.seq, t.at, t.messages FROM transcript t
		WHERE t.person = ? AND t.seq > ? AND `+notIncognito+` AND `+notDeletedThread+` ORDER BY t.seq LIMIT ?`, person, after, limit)
}
