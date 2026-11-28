package calendar

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/reminders"
)

// Scheduler 是终端那边说提醒的调度（reminders.Scheduler 实现）。
type Scheduler interface {
	ArmEvent(ctx context.Context, person, eventID string, alerts []reminders.EventAlert, text func(lead time.Duration) string) error
	DisarmEvent(ctx context.Context, eventID string) error
	Unadopted(ctx context.Context) ([]reminders.Reminder, error)
	Adopt(ctx context.Context, id int64, eventID string) error
}

// Phrases 给出到点说的固定的话（brain.Phrases.Say）。
type Phrases func(language, key string, values map[string]string) string

// Service 是日历。
type Service struct {
	store     *Store
	scheduler Scheduler // nil = 不在终端响（没开提醒那组工具）
	say       Phrases
	alertKey  string // 「提前 N 分钟」那句的键（brain.PhraseCalendarAlertAhead）
	location  *time.Location
	now       func() time.Time
	logger    *slog.Logger
}

// Options 是日历的部件。
type Options struct {
	Scheduler Scheduler
	Phrases   Phrases
	AlertKey  string
	Location  *time.Location // 重复规则里的钟点、全天的日程按哪个时区；nil = time.Local
	Now       func() time.Time
	Logger    *slog.Logger
}

// NewService 建日历。
func NewService(store *Store, opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, scheduler: opts.Scheduler, say: opts.Phrases, alertKey: opts.AlertKey,
		location: cmp.Or(opts.Location, time.Local), now: now, logger: opts.Logger.With("component", "calendar")}
}

// Input 是新建、改一条日程时给的内容。
type Input struct {
	Title, Location, Notes    string
	Start, End                time.Time
	AllDay                    bool
	Rule                      string
	Alerts                    []time.Duration
	AlertTerminal, AlertPhone bool
	Language                  string
}

// validate 校验内容；全天的按日期对齐。
func (s *Service) validate(input *Input) error {
	input.Title = strings.TrimSpace(input.Title)
	switch {
	case input.Title == "":
		return fmt.Errorf("%w：标题不能为空", ErrInvalid)
	case utf8.RuneCountInString(input.Title) > MaxTitleRunes:
		return fmt.Errorf("%w：标题最多 %d 字", ErrInvalid, MaxTitleRunes)
	case utf8.RuneCountInString(input.Location) > MaxTextRunes || utf8.RuneCountInString(input.Notes) > MaxTextRunes:
		return fmt.Errorf("%w：地点、备注最多 %d 字", ErrInvalid, MaxTextRunes)
	case len(input.Alerts) > MaxAlerts:
		return fmt.Errorf("%w：提醒最多 %d 个", ErrInvalid, MaxAlerts)
	}
	for _, lead := range input.Alerts {
		if lead < 0 || lead > MaxAlertLead {
			return fmt.Errorf("%w：提醒最多提前 %d 天", ErrInvalid, int(MaxAlertLead.Hours()/24))
		}
	}
	if input.AllDay {
		input.Start = dateOf(input.Start)
		input.End = dateOf(input.End)
		if !input.End.After(input.Start) {
			input.End = input.Start.AddDate(0, 0, 1)
		}
	} else {
		input.Start, input.End = input.Start.UTC().Truncate(time.Second), input.End.UTC().Truncate(time.Second)
		if input.End.Before(input.Start) {
			return fmt.Errorf("%w：结束不能早于开始", ErrInvalid)
		}
	}
	slices.Sort(input.Alerts)
	input.Alerts = slices.Compact(input.Alerts)
	if input.Rule != "" {
		if _, err := newExpander(Event{Start: input.Start, End: input.End, AllDay: input.AllDay, Rule: input.Rule}, s.location); err != nil {
			return err
		}
	}
	return nil
}

// dateOf 是那一天 00:00 UTC（全天的只当日期用；取的是 t 在 UTC 里的日期 —— App 交的就是那一天的 00:00 UTC）。
func dateOf(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func (s *Service) newEvent(person string, input Input) (Event, error) {
	id, err := randomID()
	if err != nil {
		return Event{}, err
	}
	now := s.now().UTC().Truncate(time.Second)
	return Event{ID: id, Person: person, Title: input.Title, Location: input.Location, Notes: input.Notes, Start: input.Start, End: input.End,
		AllDay: input.AllDay, Rule: input.Rule, Alerts: input.Alerts, AlertTerminal: input.AlertTerminal, AlertPhone: input.AlertPhone,
		Source: SourceHaro, Language: input.Language, CreatedAt: now, UpdatedAt: now}, nil
}

// Create 新建一条 haro 自己的日程。
func (s *Service) Create(ctx context.Context, person string, input Input) (Event, error) {
	if err := s.validate(&input); err != nil {
		return Event{}, err
	}
	event, err := s.newEvent(person, input)
	if err != nil {
		return Event{}, err
	}
	if err := s.store.put(ctx, event); err != nil {
		return Event{}, err
	}
	s.arm(ctx, event)
	return event, nil
}

// Get 是这个人的一条日程（删了的当成没有）。
func (s *Service) Get(ctx context.Context, person, id string) (Event, error) {
	event, err := s.store.get(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if event.Person != person || event.DeletedAt != nil {
		return Event{}, ErrNotFound
	}
	return event, nil
}

// Scope 是改、删重复日程时管到哪：All 整个系列；This 只这一次（At 是那一次的开始）；Future 这一次和以后。一次的日程只认 All。
type Scope struct {
	Kind string
	At   time.Time
}

// 管到哪。
const (
	ScopeAll    = "all"
	ScopeThis   = "this"
	ScopeFuture = "future"
)

// Update 改一条 haro 自己的日程。
func (s *Service) Update(ctx context.Context, person, id string, input Input, scope Scope) (Event, error) {
	original, err := s.Get(ctx, person, id)
	if err != nil {
		return Event{}, err
	}
	if !original.Editable() {
		return Event{}, ErrReadOnly
	}
	if err := s.validate(&input); err != nil {
		return Event{}, err
	}
	kind, err := s.scopeKind(original, scope)
	if err != nil {
		return Event{}, err
	}
	now := s.now().UTC().Truncate(time.Second)
	switch kind {
	case ScopeThis: // 原来的系列里排除这一次，另建一条单次的
		original.Exdates = append(original.Exdates, scope.At.UTC())
		original.UpdatedAt = now
		input.Rule = ""
	case ScopeFuture: // 原来的截到这一次之前，从这一次起另建一条
		if original.Rule, err = untilBefore(original.Rule, scope.At, s.ruleLocation(original)); err != nil {
			return Event{}, err
		}
		original.UpdatedAt = now
	default:
		updated := original
		updated.Title, updated.Location, updated.Notes = input.Title, input.Location, input.Notes
		updated.Start, updated.End, updated.AllDay, updated.Rule = input.Start, input.End, input.AllDay, input.Rule
		updated.Alerts, updated.AlertTerminal, updated.AlertPhone, updated.UpdatedAt = input.Alerts, input.AlertTerminal, input.AlertPhone, now
		updated.Exdates = nil
		if err := s.store.put(ctx, updated); err != nil {
			return Event{}, err
		}
		s.arm(ctx, updated)
		return updated, nil
	}
	if err := s.store.put(ctx, original); err != nil {
		return Event{}, err
	}
	s.arm(ctx, original)
	split, err := s.newEvent(person, input)
	if err != nil {
		return Event{}, err
	}
	if err := s.store.put(ctx, split); err != nil {
		return Event{}, err
	}
	s.arm(ctx, split)
	return split, nil
}

// Delete 删一条 haro 自己的日程（整个系列 / 这一次 / 这一次和以后）。
func (s *Service) Delete(ctx context.Context, person, id string, scope Scope) error {
	event, err := s.Get(ctx, person, id)
	if err != nil {
		return err
	}
	if !event.Editable() {
		return ErrReadOnly
	}
	kind, err := s.scopeKind(event, scope)
	if err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Second)
	switch kind {
	case ScopeThis:
		event.Exdates = append(event.Exdates, scope.At.UTC())
	case ScopeFuture:
		if event.Rule, err = untilBefore(event.Rule, scope.At, s.ruleLocation(event)); err != nil {
			return err
		}
	default:
		event.DeletedAt = &now
	}
	event.UpdatedAt = now
	if err := s.store.put(ctx, event); err != nil {
		return err
	}
	s.arm(ctx, event)
	return nil
}

// scopeKind 核对改、删管到哪：一次的日程、或者选的是系列的第一次的「以后」都当成整个系列；这一次 / 以后要是系列里真有的一次。
func (s *Service) scopeKind(event Event, scope Scope) (string, error) {
	if event.Rule == "" || scope.Kind == "" || scope.Kind == ScopeAll {
		return ScopeAll, nil
	}
	if scope.Kind != ScopeThis && scope.Kind != ScopeFuture {
		return "", fmt.Errorf("%w：scope 只能是 all、this、future", ErrInvalid)
	}
	if scope.Kind == ScopeFuture && !scope.At.After(event.Start) {
		return ScopeAll, nil
	}
	ex, err := newExpander(event, s.location)
	if err != nil {
		return "", err
	}
	if next, ok := ex.after(scope.At.Add(-time.Second)); !ok || !next.Equal(scope.At.UTC()) {
		return "", fmt.Errorf("%w：这条日程在那个时间没有一次", ErrInvalid)
	}
	return scope.Kind, nil
}

func (s *Service) ruleLocation(event Event) *time.Location {
	if event.AllDay {
		return time.UTC
	}
	return s.location
}

// Occurrences 是这个人和 [from, to) 有交集的每一次（haro 自己的和同步进来的，开始的先后）。
func (s *Service) Occurrences(ctx context.Context, person string, from, to time.Time) ([]Occurrence, error) {
	events, err := s.store.live(ctx, person, from, to)
	if err != nil {
		return nil, err
	}
	var out []Occurrence
	for _, event := range events {
		ex, err := newExpander(event, s.location)
		if err != nil {
			s.logger.Warn("日程的重复规则解不开：先不展开", "event", event.ID, "err", err)
			continue
		}
		out = append(out, ex.between(from, to)...)
		if len(out) >= maxOccurrences {
			break
		}
	}
	slices.SortFunc(out, func(a, b Occurrence) int { return a.Start.Compare(b.Start) })
	return out[:min(len(out), maxOccurrences)], nil
}

// alertBase 是提醒「提前多久」从哪个时刻往前算：定时的是开始；全天的是那一天在本地时区的 0 点。
func (s *Service) alertBase(event Event, start time.Time) time.Time {
	if !event.AllDay {
		return start
	}
	year, month, day := start.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, s.location).UTC()
}

// PhoneAlert 是一次要在手机上响的提醒。
type PhoneAlert struct {
	Occurrence Occurrence
	Lead       time.Duration
	At         time.Time
}

// PhoneAlerts 是这个人从现在到 until 要在手机上响的提醒（时间先后）。
func (s *Service) PhoneAlerts(ctx context.Context, person string, until time.Time) ([]PhoneAlert, error) {
	now := s.now()
	occurrences, err := s.Occurrences(ctx, person, now, until.Add(MaxAlertLead+24*time.Hour))
	if err != nil {
		return nil, err
	}
	var alerts []PhoneAlert
	for _, occurrence := range occurrences {
		if !occurrence.Event.AlertPhone || !occurrence.Event.Editable() {
			continue
		}
		for _, lead := range occurrence.Event.Alerts {
			at := s.alertBase(occurrence.Event, occurrence.Start).Add(-lead)
			if !at.Before(now) && !at.After(until) {
				alerts = append(alerts, PhoneAlert{Occurrence: occurrence, Lead: lead, At: at})
			}
		}
	}
	slices.SortFunc(alerts, func(a, b PhoneAlert) int { return a.At.Compare(b.At) })
	return alerts, nil
}

// NextAlert 实现 reminders.EventAlerts：这条日程、提前 lead 的提醒在 after 之后的下一次到点（终端那边）。
func (s *Service) NextAlert(ctx context.Context, eventID string, lead time.Duration, after time.Time) (time.Time, bool, error) {
	event, err := s.store.get(ctx, eventID)
	if err != nil {
		if err == ErrNotFound {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	if event.DeletedAt != nil || !event.AlertTerminal || !event.Editable() {
		return time.Time{}, false, nil
	}
	return s.nextAlert(event, lead, after)
}

func (s *Service) nextAlert(event Event, lead time.Duration, after time.Time) (time.Time, bool, error) {
	ex, err := newExpander(event, s.location)
	if err != nil {
		return time.Time{}, false, err
	}
	// 往后找第一次「提醒时刻在 after 之后」的：提醒时刻 = 那一次的基准 - lead
	cursor := after.Add(lead).Add(-25 * time.Hour) // 全天的基准是本地 0 点，和 UTC 的日期差不到一天
	for range maxOccurrences {
		start, ok := ex.after(cursor)
		if !ok {
			return time.Time{}, false, nil
		}
		if at := s.alertBase(event, start).Add(-lead); at.After(after) {
			return at, true, nil
		}
		cursor = start
	}
	return time.Time{}, false, nil
}

// arm 按日程重新挂终端那边的提醒（删了的、不在终端响的、没有提醒的、同步进来的：都取消）。出错只记日志：日程照样存下了。
func (s *Service) arm(ctx context.Context, event Event) {
	if s.scheduler == nil {
		return
	}
	var alerts []reminders.EventAlert
	if event.DeletedAt == nil && event.AlertTerminal && event.Editable() {
		now := s.now()
		for _, lead := range event.Alerts {
			next, ok, err := s.nextAlert(event, lead, now)
			if err != nil {
				s.logger.Warn("算日程的下一次提醒出错", "event", event.ID, "err", err)
				continue
			}
			if ok {
				alerts = append(alerts, reminders.EventAlert{Lead: lead, Next: next})
			}
		}
	}
	var err error
	if len(alerts) == 0 {
		err = s.scheduler.DisarmEvent(ctx, event.ID)
	} else {
		err = s.scheduler.ArmEvent(ctx, event.Person, event.ID, alerts, func(lead time.Duration) string { return s.alertText(event, lead) })
	}
	if err != nil {
		s.logger.Error("挂终端那边的提醒出错：这条日程到点可能不会在终端响", "event", event.ID, "err", err)
	}
}

// alertText 是到点在终端说的话：提前 0 = 标题；提前 N 分钟 = 固定句式（按日程建的时候的语言）。
func (s *Service) alertText(event Event, lead time.Duration) string {
	if lead == 0 || s.say == nil {
		return event.Title
	}
	return s.say(event.Language, s.alertKey, map[string]string{"title": event.Title, "minutes": strconv.Itoa(int(lead.Minutes()))})
}

// SyncedEvent 是 App 交来的系统日历里的一次（重复的由系统展开好了，每次一条，编号各不相同）。
type SyncedEvent struct {
	ExternalID             string
	Title, Location, Notes string
	Start, End             time.Time
	AllDay                 bool
}

// SyncResult 是同步的结果：新增、改了、删了几条。
type SyncResult struct{ Added, Updated, Removed int }

// Sync 收下一个系统日历在同步范围内的全部日程：按系统的编号增、改；这个日历原来有、这次没交来的删掉（只读，不在终端响）。
func (s *Service) Sync(ctx context.Context, person, calendar, name string, events []SyncedEvent) (SyncResult, error) {
	var result SyncResult
	now := s.now().UTC().Truncate(time.Second)
	seen := map[string]bool{}
	for _, synced := range events {
		if synced.ExternalID == "" || seen[synced.ExternalID] {
			continue
		}
		seen[synced.ExternalID] = true
		input := Input{Title: synced.Title, Location: synced.Location, Notes: synced.Notes, Start: synced.Start, End: synced.End, AllDay: synced.AllDay}
		if strings.TrimSpace(input.Title) == "" {
			input.Title = "—"
		}
		if err := s.validate(&input); err != nil {
			continue // 系统日历里写得不合规的那一条跳过，不让整批失败
		}
		existing, err := s.store.syncedOne(ctx, person, calendar, synced.ExternalID)
		switch {
		case err == ErrNotFound:
			id, err := randomID()
			if err != nil {
				return result, err
			}
			event := Event{ID: id, Person: person, Title: input.Title, Location: input.Location, Notes: input.Notes, Start: input.Start,
				End: input.End, AllDay: input.AllDay, Source: SourceSystem, Calendar: calendar, CalendarName: name,
				ExternalID: synced.ExternalID, CreatedAt: now, UpdatedAt: now}
			if err := s.store.put(ctx, event); err != nil {
				return result, err
			}
			result.Added++
		case err != nil:
			return result, err
		default:
			changed := existing.Title != input.Title || existing.Location != input.Location || existing.Notes != input.Notes ||
				!existing.Start.Equal(input.Start) || !existing.End.Equal(input.End) || existing.AllDay != input.AllDay ||
				existing.DeletedAt != nil || existing.CalendarName != name
			if !changed {
				continue
			}
			existing.Title, existing.Location, existing.Notes = input.Title, input.Location, input.Notes
			existing.Start, existing.End, existing.AllDay, existing.CalendarName = input.Start, input.End, input.AllDay, name
			existing.DeletedAt, existing.UpdatedAt = nil, now
			if err := s.store.put(ctx, existing); err != nil {
				return result, err
			}
			result.Updated++
		}
	}
	current, err := s.store.synced(ctx, person, calendar)
	if err != nil {
		return result, err
	}
	for _, event := range current {
		if seen[event.ExternalID] {
			continue
		}
		event.DeletedAt, event.UpdatedAt = &now, now
		if err := s.store.put(ctx, event); err != nil {
			return result, err
		}
		result.Removed++
	}
	if err := s.store.recordSync(ctx, person, calendar, name, now); err != nil {
		return result, err
	}
	s.logger.Info("系统日历同步了", "person", person, "calendar", name, "added", result.Added, "updated", result.Updated, "removed", result.Removed)
	return result, nil
}

// Unsync 不再同步这个系统日历：它同步进来的日程去掉（软删除），同步记录也删掉；没同步过时返回 ErrNotFound。
func (s *Service) Unsync(ctx context.Context, person, calendar string) error {
	removed, err := s.store.unsync(ctx, person, calendar, s.now().UTC().Truncate(time.Second))
	if err != nil {
		return err
	}
	s.logger.Info("不再同步系统日历", "person", person, "calendar", calendar, "removed", removed)
	return nil
}

// Syncs 是这个人同步过的系统日历（上次同步的时间）。
func (s *Service) Syncs(ctx context.Context, person string) ([]Sync, error) {
	return s.store.syncs(ctx, person)
}

// Adopt 把还没挂到日程上的旧提醒收编成日程（标题 = 原来到点要说的话、开始 = 第一次到点、规则照搬），幂等：启动时做一次。
func (s *Service) Adopt(ctx context.Context) error {
	if s.scheduler == nil {
		return nil
	}
	old, err := s.scheduler.Unadopted(ctx)
	if err != nil {
		return err
	}
	for _, reminder := range old {
		event, err := s.eventForReminder(reminder.Person, reminder.Text, reminder.StartsAt, reminder.Rule)
		if err != nil {
			return err
		}
		if err := s.store.put(ctx, event); err != nil {
			return err
		}
		if err := s.scheduler.Adopt(ctx, reminder.ID, event.ID); err != nil {
			return err
		}
	}
	if len(old) > 0 {
		s.logger.Info("原来的提醒收编成了日程", "count", len(old))
	}
	return nil
}

// eventForReminder 是语音定的一条提醒对应的日程：标题是到点要说的话，开始就是到点，终端、手机都响（提前 0）。
func (s *Service) eventForReminder(person, text string, at time.Time, rule string) (Event, error) {
	id, err := randomID()
	if err != nil {
		return Event{}, err
	}
	now := s.now().UTC().Truncate(time.Second)
	return Event{ID: id, Person: person, Title: text, Start: at.UTC(), End: at.UTC(), Rule: rule, Alerts: []time.Duration{0},
		AlertTerminal: true, AlertPhone: true, Source: SourceHaro, Origin: OriginReminder, CreatedAt: now, UpdatedAt: now}, nil
}

// OriginReminder 是语音定提醒时建的日程（取消提醒时跟着删、恢复时跟着恢复）。
const OriginReminder = "reminder"

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ErasePerson 删掉这个人的全部日程和同步记录（彻底删账号时用；终端那边的提醒由提醒的调度自己删）。
func (s *Service) ErasePerson(ctx context.Context, person string) error {
	return s.store.ErasePerson(ctx, person)
}
