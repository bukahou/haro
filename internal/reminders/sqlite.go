package reminders

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

// SQLiteStore 是 Store 的 SQLite 实现。和记事本放在同一个库文件里（v9 D7：记事库里加一张表），
// 表结构的版本记在自己的表 goose_db_version_reminders：两边的迁移各管各的。
type SQLiteStore struct {
	db *sql.DB
}

var _ Store = (*SQLiteStore)(nil)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// versionTable 是提醒表结构的版本记在哪张表（不和记事本的 goose_db_version 混）。
const versionTable = "goose_db_version_reminders"

// OpenSQLite 打开（必要时创建）库文件并把提醒表升到最新。path 为空 = 只在内存里。
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	db, err := sqliteschema.Open(ctx, path, migrationFiles,
		sqliteschema.Options{Migrate: []goose.ProviderOption{goose.WithTableName(versionTable)}})
	if err != nil {
		return nil, fmt.Errorf("打开提醒的数据库 %s: %w", path, err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close 关闭数据库。
func (s *SQLiteStore) Close() error { return s.db.Close() }

const columns = `id, person, text, rrule, starts_at, next_at, created_at, cancelled_at, spoken_at, missed_at, event_id, lead_seconds`

func (s *SQLiteStore) Add(ctx context.Context, r Reminder) (Reminder, error) {
	r.Text = strings.TrimSpace(r.Text)
	if r.Person == "" || r.Text == "" || r.StartsAt.IsZero() || r.NextAt.IsZero() {
		return Reminder{}, errors.New("提醒缺少人、要说的话或时间")
	}
	r.CreatedAt = time.Now().UTC().Truncate(time.Second)
	result, err := s.db.ExecContext(ctx, `INSERT INTO reminders (person, text, rrule, starts_at, next_at, created_at, event_id, lead_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.Person, r.Text, nullString(r.Rule), r.StartsAt.Unix(), r.NextAt.Unix(), r.CreatedAt.Unix(),
		nullString(r.EventID), int64(r.Lead.Seconds()))
	if err != nil {
		return Reminder{}, err
	}
	r.ID, err = result.LastInsertId()
	r.StartsAt, r.NextAt = r.StartsAt.UTC().Truncate(time.Second), r.NextAt.UTC().Truncate(time.Second)
	return r, err
}

func (s *SQLiteStore) CountActive(ctx context.Context, person string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reminders
		WHERE person = ? AND cancelled_at IS NULL AND next_at IS NOT NULL`, person).Scan(&count)
	return count, err
}

func (s *SQLiteStore) Active(ctx context.Context, person string) ([]Reminder, error) {
	return s.query(ctx, `SELECT `+columns+` FROM reminders WHERE person = ? AND cancelled_at IS NULL AND next_at IS NOT NULL
		ORDER BY next_at, id`, person)
}

func (s *SQLiteStore) Cancelled(ctx context.Context, person string, limit int) ([]Reminder, error) {
	return s.query(ctx, `SELECT `+columns+` FROM reminders WHERE person = ? AND cancelled_at IS NOT NULL
		ORDER BY cancelled_at DESC, id DESC LIMIT ?`, person, limit)
}

func (s *SQLiteStore) Get(ctx context.Context, person string, id int64) (Reminder, error) {
	list, err := s.query(ctx, `SELECT `+columns+` FROM reminders WHERE person = ? AND id = ?`, person, id)
	if err != nil {
		return Reminder{}, err
	}
	if len(list) == 0 {
		return Reminder{}, ErrNotFound
	}
	return list[0], nil
}

func (s *SQLiteStore) Cancel(ctx context.Context, person string, id int64, at time.Time) (Reminder, error) {
	return s.change(ctx, person, id, `UPDATE reminders SET cancelled_at = ? WHERE person = ? AND id = ? AND cancelled_at IS NULL`,
		at.Unix(), person, id)
}

func (s *SQLiteStore) Restore(ctx context.Context, person string, id int64, next time.Time) (Reminder, error) {
	return s.change(ctx, person, id, `UPDATE reminders SET cancelled_at = NULL, next_at = ?
		WHERE person = ? AND id = ? AND cancelled_at IS NOT NULL`, next.Unix(), person, id)
}

// ForEvent 是一条日程在用的提醒（没取消的）。
func (s *SQLiteStore) ForEvent(ctx context.Context, eventID string) ([]Reminder, error) {
	return s.query(ctx, `SELECT `+columns+` FROM reminders WHERE event_id = ? AND cancelled_at IS NULL ORDER BY lead_seconds DESC, id`, eventID)
}

// CancelForEvent 取消一条日程在用的提醒（日程改了、删了）。
func (s *SQLiteStore) CancelForEvent(ctx context.Context, eventID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET cancelled_at = ? WHERE event_id = ? AND cancelled_at IS NULL`, at.Unix(), eventID)
	return err
}

// Unadopted 是还没挂到日程上的在用提醒（旧的；日历启动时收编）。
func (s *SQLiteStore) Unadopted(ctx context.Context) ([]Reminder, error) {
	return s.query(ctx, `SELECT `+columns+` FROM reminders WHERE event_id IS NULL AND cancelled_at IS NULL ORDER BY id`)
}

// Adopt 把一条旧提醒挂到日程上：记下 event_id，规则清掉（之后下一次到点问日历）。
func (s *SQLiteStore) Adopt(ctx context.Context, id int64, eventID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET event_id = ?, rrule = NULL WHERE id = ? AND event_id IS NULL`, eventID, id)
	return err
}

// change 改一条（只改符合条件的那条），看影响行数决定有没有这一条（G5），返回改完的样子。
func (s *SQLiteStore) change(ctx context.Context, person string, id int64, statement string, args ...any) (Reminder, error) {
	result, err := s.db.ExecContext(ctx, statement, args...)
	if err != nil {
		return Reminder{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return Reminder{}, cmpErr(err, ErrNotFound)
	}
	return s.Get(ctx, person, id)
}

func (s *SQLiteStore) Due(ctx context.Context, at time.Time) ([]Reminder, error) {
	return s.query(ctx, `SELECT `+columns+` FROM reminders WHERE cancelled_at IS NULL AND next_at IS NOT NULL AND next_at <= ?
		ORDER BY next_at, id`, at.Unix())
}

func (s *SQLiteStore) NextDue(ctx context.Context) (time.Time, bool, error) {
	var next sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(next_at) FROM reminders WHERE cancelled_at IS NULL`).Scan(&next); err != nil {
		return time.Time{}, false, err
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(next.Int64, 0).UTC(), true, nil
}

func (s *SQLiteStore) Spoken(ctx context.Context, id int64, at, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET spoken_at = ?, next_at = ? WHERE id = ? AND cancelled_at IS NULL`,
		at.Unix(), nullTime(next), id)
	return err
}

func (s *SQLiteStore) Missed(ctx context.Context, id int64, missedAt, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET missed_at = ?, next_at = ? WHERE id = ? AND cancelled_at IS NULL`,
		missedAt.Unix(), nullTime(next), id)
	return err
}

func (s *SQLiteStore) Unannounced(ctx context.Context, person string) ([]Reminder, error) {
	return s.query(ctx, `SELECT `+columns+` FROM reminders WHERE person = ? AND cancelled_at IS NULL AND missed_at IS NOT NULL
		ORDER BY missed_at, id`, person)
}

func (s *SQLiteStore) MissTold(ctx context.Context, ids ...int64) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE reminders SET missed_at = NULL WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) query(ctx context.Context, statement string, args ...any) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Reminder
	for rows.Next() {
		var r Reminder
		var rule, event sql.NullString
		var starts, created, lead int64
		var next, cancelled, spoken, missed sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Person, &r.Text, &rule, &starts, &next, &created, &cancelled, &spoken, &missed, &event, &lead); err != nil {
			return nil, err
		}
		r.Rule, r.EventID, r.Lead = rule.String, event.String, time.Duration(lead)*time.Second
		r.StartsAt, r.CreatedAt = time.Unix(starts, 0).UTC(), time.Unix(created, 0).UTC()
		r.NextAt, r.CancelledAt, r.SpokenAt, r.MissedAt = fromNull(next), fromNull(cancelled), fromNull(spoken), fromNull(missed)
		list = append(list, r)
	}
	return list, rows.Err()
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func nullTime(t time.Time) sql.NullInt64 { return sql.NullInt64{Int64: t.Unix(), Valid: !t.IsZero()} }

func fromNull(value sql.NullInt64) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return time.Unix(value.Int64, 0).UTC()
}

// cmpErr：有错误就交出错误，没有就交出 fallback。
func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
