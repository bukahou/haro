package calendar

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/bukahou/haro/internal/sqliteschema"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// versionTable 是日历表结构的版本记在哪张表（和提醒、记事的分开）。
const versionTable = "goose_db_version_calendar"

// Store 把日程存在 SQLite（和提醒、记事同一个库文件）。
type Store struct{ db *sql.DB }

// OpenStore 打开（没有就建）库文件，把日历的表升到最新。path 为空 = 只在内存里。
func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sqliteschema.Open(ctx, path, migrationFiles,
		sqliteschema.Options{Migrate: []goose.ProviderOption{goose.WithTableName(versionTable)}})
	if err != nil {
		return nil, fmt.Errorf("打开日历的数据库 %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

const eventColumns = `id, person, title, location, notes, starts_at, ends_at, all_day, rrule, exdates, alerts, alert_terminal, alert_phone,
	source, calendar, calendar_name, external_id, origin, language, created_at, updated_at, deleted_at`

// put 存一条（新的或整条替换）。
func (s *Store) put(ctx context.Context, event Event) error {
	exdates, alerts, err := encodeLists(event)
	if err != nil {
		return err
	}
	var rule, deleted any
	if event.Rule != "" {
		rule = event.Rule
	}
	if event.DeletedAt != nil {
		deleted = event.DeletedAt.Unix()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO events (`+eventColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET title = excluded.title, location = excluded.location, notes = excluded.notes,
			starts_at = excluded.starts_at, ends_at = excluded.ends_at, all_day = excluded.all_day, rrule = excluded.rrule,
			exdates = excluded.exdates, alerts = excluded.alerts, alert_terminal = excluded.alert_terminal, alert_phone = excluded.alert_phone,
			calendar_name = excluded.calendar_name, updated_at = excluded.updated_at, deleted_at = excluded.deleted_at`,
		event.ID, event.Person, event.Title, event.Location, event.Notes, event.Start.Unix(), event.End.Unix(), event.AllDay, rule,
		exdates, alerts, event.AlertTerminal, event.AlertPhone, event.Source, event.Calendar, event.CalendarName, event.ExternalID,
		event.Origin, event.Language, event.CreatedAt.Unix(), event.UpdatedAt.Unix(), deleted)
	if err != nil {
		return fmt.Errorf("存日程: %w", err)
	}
	return nil
}

func encodeLists(event Event) (exdates, alerts string, err error) {
	ex := make([]int64, 0, len(event.Exdates))
	for _, at := range event.Exdates {
		ex = append(ex, at.Unix())
	}
	leads := make([]int64, 0, len(event.Alerts))
	for _, lead := range event.Alerts {
		leads = append(leads, int64(lead.Seconds()))
	}
	exData, err := json.Marshal(ex)
	if err != nil {
		return "", "", err
	}
	leadData, err := json.Marshal(leads)
	return string(exData), string(leadData), err
}

// get 是一条日程（删了的也给）。
func (s *Store) get(ctx context.Context, id string) (Event, error) {
	events, err := s.query(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, id)
	if err != nil {
		return Event{}, err
	}
	if len(events) == 0 {
		return Event{}, ErrNotFound
	}
	return events[0], nil
}

// live 是这个人在用的日程（没删的），可能和 [from, to) 有交集的：一次的看时间，重复的都给（展开时再筛）。
func (s *Store) live(ctx context.Context, person string, from, to time.Time) ([]Event, error) {
	return s.query(ctx, `SELECT `+eventColumns+` FROM events WHERE person = ? AND deleted_at IS NULL
		AND (rrule IS NOT NULL OR (starts_at < ? AND ends_at > ?)) AND starts_at < ? ORDER BY starts_at`,
		person, to.Unix(), from.Unix(), to.Unix())
}

// synced 是这个人从一个系统日历同步进来、还在用的日程。
func (s *Store) synced(ctx context.Context, person, calendar string) ([]Event, error) {
	return s.query(ctx, `SELECT `+eventColumns+` FROM events WHERE person = ? AND source = ? AND calendar = ? AND deleted_at IS NULL`,
		person, SourceSystem, calendar)
}

// syncedOne 是系统日历里的一条（按系统的编号；删了的也给）。
func (s *Store) syncedOne(ctx context.Context, person, calendar, externalID string) (Event, error) {
	events, err := s.query(ctx, `SELECT `+eventColumns+` FROM events WHERE person = ? AND source = ? AND calendar = ? AND external_id = ?`,
		person, SourceSystem, calendar, externalID)
	if err != nil {
		return Event{}, err
	}
	if len(events) == 0 {
		return Event{}, ErrNotFound
	}
	return events[0], nil
}

// recordSync 记下一个系统日历同步的时间。
func (s *Store) recordSync(ctx context.Context, person, calendar, name string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO calendar_syncs (person, calendar, name, synced_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (person, calendar) DO UPDATE SET name = excluded.name, synced_at = excluded.synced_at`, person, calendar, name, at.Unix())
	return err
}

// Sync 是一个系统日历最近一次同步。
type Sync struct {
	Calendar, Name string
	SyncedAt       time.Time
}

// syncs 是这个人同步过的系统日历。
func (s *Store) syncs(ctx context.Context, person string) ([]Sync, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT calendar, name, synced_at FROM calendar_syncs WHERE person = ? ORDER BY name`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var syncs []Sync
	for rows.Next() {
		var sync Sync
		var at int64
		if err := rows.Scan(&sync.Calendar, &sync.Name, &at); err != nil {
			return nil, err
		}
		sync.SyncedAt = time.Unix(at, 0).UTC()
		syncs = append(syncs, sync)
	}
	return syncs, rows.Err()
}

func (s *Store) query(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("读日程: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var starts, ends, created, updated int64
		var rule sql.NullString
		var exdates, alerts string
		var deleted sql.NullInt64
		if err := rows.Scan(&event.ID, &event.Person, &event.Title, &event.Location, &event.Notes, &starts, &ends, &event.AllDay, &rule,
			&exdates, &alerts, &event.AlertTerminal, &event.AlertPhone, &event.Source, &event.Calendar, &event.CalendarName, &event.ExternalID,
			&event.Origin, &event.Language, &created, &updated, &deleted); err != nil {
			return nil, fmt.Errorf("读日程: %w", err)
		}
		event.Start, event.End = time.Unix(starts, 0).UTC(), time.Unix(ends, 0).UTC()
		event.CreatedAt, event.UpdatedAt, event.Rule = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC(), rule.String
		if deleted.Valid {
			at := time.Unix(deleted.Int64, 0).UTC()
			event.DeletedAt = &at
		}
		var ex, leads []int64
		if err := errors.Join(json.Unmarshal([]byte(exdates), &ex), json.Unmarshal([]byte(alerts), &leads)); err != nil {
			return nil, fmt.Errorf("日程 %s 的排除日期 / 提醒解不开: %w", event.ID, err)
		}
		for _, at := range ex {
			event.Exdates = append(event.Exdates, time.Unix(at, 0).UTC())
		}
		for _, lead := range leads {
			event.Alerts = append(event.Alerts, time.Duration(lead)*time.Second)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
