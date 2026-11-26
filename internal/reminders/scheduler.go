package reminders

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/telemetry"
)

// Announcer 把一句话交给这个人最近说话的那台终端说出来（brain.NoticeHub 实现）。交出去了返回 true，
// 之后恰好调用一次 Done；没有在线的终端时返回 false。
type Announcer interface {
	Announce(announcement brain.Announcement) bool
}

// Scheduler 管提醒：定、查、取消、恢复（给工具用），到点交给 Announcer 说出来（Run），
// 错过的在主人下次开口时附在情况说明里（Brief，实现 brain.Briefer）。
type Scheduler struct {
	store     Store
	announcer Announcer
	location  *time.Location
	now       func() time.Time
	retry     time.Duration
	logger    *slog.Logger

	wake chan struct{} // 有变化（定了、取消、恢复、开口有了结果、有终端连上）：调度循环重新看一遍

	mu       sync.Mutex
	inFlight map[int64]time.Time // 已经交出去、还没有结果的提醒 → 交出去的是哪一次到点
	waiting  map[int64]bool      // 到点了、交不出去（没有在线的终端）的提醒：之后说出来算补说；日志只记第一次
}

// SchedulerOptions 是调度的可调部分；零值的字段用默认值。
type SchedulerOptions struct {
	Location *time.Location   // 推算钟点用的时区；nil = time.Local（服务端的 TZ）
	Now      func() time.Time // 现在几点；nil = time.Now（测试用）
	// Retry：到点了却交不出去时，隔多久再试一次（终端连上时会马上再试，这个只兜底）；零值 = defaultRetry
	Retry time.Duration
}

// defaultRetry：交不出去的提醒隔这么久再试。终端连上时 NoticeHub 会马上叫醒调度，这里只兜底；定值
const defaultRetry = 30 * time.Second

// maxSleep：调度循环最长睡这么久就醒来看一眼（系统时间被调过时不至于一直睡过头）；定值
const maxSleep = time.Hour

// NewScheduler 创建调度。Run 之后才会到点开口；定、查、取消、恢复随时可用。
func NewScheduler(store Store, announcer Announcer, opts SchedulerOptions, logger *slog.Logger) *Scheduler {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Scheduler{store: store, announcer: announcer, location: cmp.Or(opts.Location, time.Local),
		now: now, retry: cmp.Or(opts.Retry, defaultRetry),
		logger: logger.With("component", "reminders"), wake: make(chan struct{}, 1),
		inFlight: map[int64]time.Time{}, waiting: map[int64]bool{}}
}

// Poke 让调度循环马上重新看一遍（不阻塞）。例如有终端连上了：等着终端的提醒可以说了。
func (s *Scheduler) Poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Request 是定一条提醒：After 和 At 恰好给一个；Rule 可选（重复规则，从第一次到点起算）。
type Request struct {
	Text  string
	After time.Duration // 多久以后（> 0）
	At    time.Time     // 什么时候（带时区）
	Rule  string        // RFC 5545 RRULE；空 = 一次性
}

// Set 给 person 定一条提醒。校验：要说的话不空、不太长；时间二选一、在将来、不超过 MaxAhead；重复规则合法；
// 在用的不超过 MaxActivePerPerson。
func (s *Scheduler) Set(ctx context.Context, person string, request Request) (Reminder, error) {
	now := s.now()
	if request.Text == "" {
		return Reminder{}, errors.New("text 不能为空：写到点时要说的那句话")
	}
	if runes := utf8.RuneCountInString(request.Text); runes > MaxTextRunes {
		return Reminder{}, fmt.Errorf("text 太长（%d 字），到点要说的话最多 %d 字", runes, MaxTextRunes)
	}
	var first time.Time
	switch {
	case request.After > 0 && !request.At.IsZero(), request.After <= 0 && request.At.IsZero():
		return Reminder{}, errors.New("after_seconds 和 at 要恰好给一个")
	case request.After > 0:
		first = now.Add(request.After)
	default:
		first = request.At
	}
	first = first.UTC().Truncate(time.Second)
	switch {
	case !first.After(now):
		return Reminder{}, errors.New("这个时间已经过了：按现在的时间重新算")
	case first.Sub(now) > MaxAhead:
		return Reminder{}, fmt.Errorf("最远只能定到 %d 天以后", int(MaxAhead.Hours()/24))
	}
	if request.Rule != "" {
		if _, err := parseRecurrence(request.Rule, first, s.location); err != nil {
			return Reminder{}, err
		}
	}
	if count, err := s.store.CountActive(ctx, person); err != nil {
		return Reminder{}, err
	} else if count >= MaxActivePerPerson {
		return Reminder{}, fmt.Errorf("在用的提醒已经有 %d 条（最多 %d 条）：先取消用不着的", count, MaxActivePerPerson)
	}
	reminder, err := s.store.Add(ctx, Reminder{Person: person, Text: request.Text, Rule: request.Rule, StartsAt: first, NextAt: first})
	if err != nil {
		return Reminder{}, err
	}
	s.logger.Info("定了提醒", "id", reminder.ID, "person", person, "next", reminder.NextAt.In(s.location), "rule", reminder.Rule)
	s.Poke()
	return reminder, nil
}

// Listing 是一个人的提醒：在用的（按下一次到点），和最近取消的（要的时候才列）。
type Listing struct {
	Active    []Reminder
	Cancelled []Reminder
}

// List 列出 person 在用的提醒；withCancelled 时再列出最近取消的（找回用）。
func (s *Scheduler) List(ctx context.Context, person string, withCancelled bool) (Listing, error) {
	active, err := s.store.Active(ctx, person)
	if err != nil {
		return Listing{}, err
	}
	listing := Listing{Active: active}
	if withCancelled {
		listing.Cancelled, err = s.store.Cancelled(ctx, person, cancelledListLimit)
	}
	return listing, err
}

// Cancel 取消 person 的一条提醒（软删除，可以恢复）。没有这一条（或已经取消了）返回 ErrNotFound。
func (s *Scheduler) Cancel(ctx context.Context, person string, id int64) (Reminder, error) {
	reminder, err := s.store.Cancel(ctx, person, id, s.now())
	if err == nil {
		s.logger.Info("取消了提醒", "id", id, "person", person)
		s.Poke()
	}
	return reminder, err
}

// Restore 恢复 person 取消了的一条提醒：一次性的时间已经过了就不能恢复（要重新定）；重复的从现在起按规则接着算。
func (s *Scheduler) Restore(ctx context.Context, person string, id int64) (Reminder, error) {
	reminder, err := s.store.Get(ctx, person, id)
	if err != nil {
		return Reminder{}, err
	}
	if reminder.CancelledAt.IsZero() {
		return Reminder{}, ErrNotFound
	}
	now := s.now()
	next := reminder.NextAt
	if next.IsZero() || !next.After(now) {
		if !reminder.Repeats() {
			return Reminder{}, errors.New("这个提醒的时间已经过了，不能恢复：要的话重新定一个")
		}
		rule, err := parseRecurrence(reminder.Rule, reminder.StartsAt, s.location)
		if err != nil {
			return Reminder{}, err
		}
		var ok bool
		if next, ok = rule.after(now); !ok {
			return Reminder{}, errors.New("这个重复的提醒已经到头了，不能恢复：要的话重新定一个")
		}
	}
	if count, err := s.store.CountActive(ctx, person); err != nil {
		return Reminder{}, err
	} else if count >= MaxActivePerPerson {
		return Reminder{}, fmt.Errorf("在用的提醒已经有 %d 条（最多 %d 条）：先取消用不着的", count, MaxActivePerPerson)
	}
	reminder, err = s.store.Restore(ctx, person, id, next)
	if err == nil {
		s.logger.Info("恢复了提醒", "id", id, "person", person, "next", next.In(s.location))
		s.Poke()
	}
	return reminder, err
}

// Location 是推算钟点、给模型看时间用的时区。
func (s *Scheduler) Location() *time.Location { return s.location }

// Now 是调度眼里的现在（测试里可以换）。
func (s *Scheduler) Now() time.Time { return s.now() }

// Run 到点开口，直到 ctx 结束。服务端重启后先把到了点的处理掉（CatchUpWindow 以内的补说，更早的记成错过）。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		sleep := s.handleDue(ctx)
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// handleDue 处理到了点的提醒，返回睡多久再看。读库出错时记警告，过 retry 再试。
func (s *Scheduler) handleDue(ctx context.Context) time.Duration {
	now := s.now()
	due, err := s.store.Due(ctx, now)
	if err != nil {
		s.logger.Warn("读到点的提醒失败：过一会儿再试", "err", err)
		return s.retry
	}
	waiting := false
	for _, reminder := range due {
		s.mu.Lock()
		_, busy := s.inFlight[reminder.ID]
		s.mu.Unlock()
		if busy {
			continue
		}
		if late := now.Sub(reminder.NextAt); late > CatchUpWindow {
			s.missed(ctx, reminder, now)
			continue
		}
		if !s.announce(reminder) {
			waiting = true
		}
	}
	next, ok, err := s.store.NextDue(ctx)
	switch {
	case err != nil:
		s.logger.Warn("读下一次到点失败：过一会儿再试", "err", err)
		return s.retry
	case ok:
		nextDue.Record(ctx, float64(next.Unix()))
	default:
		nextDue.Record(ctx, 0)
	}
	sleep := maxSleep
	if ok && next.After(now) {
		sleep = min(sleep, next.Sub(now))
	}
	if waiting {
		sleep = min(sleep, s.retry)
	}
	return sleep
}

// announce 把到了点的这一次交给 Announcer；交不出去（没有在线的终端）返回 false，之后再试。
func (s *Scheduler) announce(reminder Reminder) bool {
	occurrence := reminder.NextAt
	s.mu.Lock()
	s.inFlight[reminder.ID] = occurrence
	s.mu.Unlock()
	delivered := s.announcer.Announce(brain.Announcement{
		Reason: realtimeproto.UnpromptedReminder, Person: reminder.Person, Text: reminder.Text,
		Note: fmt.Sprintf("到点提醒 #%d（定的时间是 %s）：下面这句是 haro 自己说出来的提醒，不是在回答主人。",
			reminder.ID, reminder.NextAt.In(s.location).Format("1月2日 15:04")),
		Done: func(outcome brain.AnnouncementOutcome) { s.finished(reminder, occurrence, outcome) },
	})
	if delivered {
		return true
	}
	s.mu.Lock()
	delete(s.inFlight, reminder.ID)
	first := !s.waiting[reminder.ID]
	s.waiting[reminder.ID] = true
	s.mu.Unlock()
	if first {
		s.logger.Warn("提醒到点了，没有在线的终端：终端连上就补说", "id", reminder.ID, "person", reminder.Person,
			"due", occurrence.In(s.location), "catch_up_until", occurrence.Add(CatchUpWindow).In(s.location))
	}
	return false
}

// finished 是一次开口的结果（在会话的 goroutine 里调用）：说出来了（或改播了提示音）就记下、算下一次；
// 没能说就留着，调度循环之后再试（还在 CatchUpWindow 以内的话）。
func (s *Scheduler) finished(reminder Reminder, occurrence time.Time, outcome brain.AnnouncementOutcome) {
	s.mu.Lock()
	delete(s.inFlight, reminder.ID)
	caughtUp := s.waiting[reminder.ID]
	if outcome != brain.AnnouncementDropped {
		delete(s.waiting, reminder.ID)
	}
	s.mu.Unlock()
	defer s.Poke()
	if outcome == brain.AnnouncementDropped {
		s.logger.Warn("提醒没能说出来：之后再试", "id", reminder.ID)
		return
	}
	now := s.now()
	caughtUp = caughtUp || now.Sub(occurrence) > caughtUpLate
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	next := s.nextAfter(reminder, occurrence, now)
	if err := s.store.Spoken(ctx, reminder.ID, now, next); err != nil {
		// 没记下：下一轮会把它当成还没说、再说一遍（宁可多说一遍，不能悄悄不说）
		s.logger.Error("记下提醒已说出失败：可能会再说一遍", "id", reminder.ID, "err", err)
		return
	}
	spoken.Add(ctx, 1, metric.WithAttributes(attribute.Bool("haro.reminder.chimed", outcome == brain.AnnouncementChimed),
		attribute.Bool("haro.reminder.caught_up", caughtUp)))
	s.logger.Info("提醒说出来了", "id", reminder.ID, "chimed", outcome == brain.AnnouncementChimed, "caught_up", caughtUp,
		"next", formatNext(next, s.location))
}

// missed 记下这一次错过了（到点后超过 CatchUpWindow 还没说出来）：主人下次开口时告诉他；重复的接着算下一次。
func (s *Scheduler) missed(ctx context.Context, reminder Reminder, now time.Time) {
	next := s.nextAfter(reminder, reminder.NextAt, now)
	if err := s.store.Missed(ctx, reminder.ID, reminder.NextAt, next); err != nil {
		s.logger.Error("记下提醒错过了失败", "id", reminder.ID, "err", err)
		return
	}
	s.mu.Lock()
	delete(s.waiting, reminder.ID)
	s.mu.Unlock()
	missedCount.Add(ctx, 1)
	s.logger.Warn("提醒错过了（一直没有在线的终端）：主人下次开口时告诉他", "id", reminder.ID, "person", reminder.Person,
		"due", reminder.NextAt.In(s.location), "next", formatNext(next, s.location))
}

// nextAfter 是这一次（occurrence）之后、现在之后的下一次到点；一次性的、或规则到头了为零值。
func (s *Scheduler) nextAfter(reminder Reminder, occurrence, now time.Time) time.Time {
	if !reminder.Repeats() {
		return time.Time{}
	}
	rule, err := parseRecurrence(reminder.Rule, reminder.StartsAt, s.location)
	if err != nil { // 定的时候校验过：只会是库里的数据坏了
		s.logger.Error("提醒的重复规则解析失败：不再重复", "id", reminder.ID, "rule", reminder.Rule, "err", err)
		return time.Time{}
	}
	from := occurrence
	if now.After(from) {
		from = now
	}
	next, ok := rule.after(from)
	if !ok {
		return time.Time{}
	}
	return next
}

// Brief 实现 brain.Briefer：这个人错过了的提醒，附在这一轮的情况说明里；他听完这一轮才标成已告诉。
func (s *Scheduler) Brief(person string) brain.Briefing {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	missed, err := s.store.Unannounced(ctx, person)
	if err != nil {
		s.logger.Warn("读错过的提醒失败：这一轮不提", "person", person, "err", err)
		return brain.Briefing{}
	}
	if len(missed) == 0 {
		return brain.Briefing{}
	}
	lines := make([]string, 0, len(missed))
	ids := make([]int64, 0, len(missed))
	for _, reminder := range missed {
		lines = append(lines, fmt.Sprintf("提醒 #%d「%s」本该在 %s 说，那时没有在线的终端，没能说出来。这一轮顺便告诉主人一声。",
			reminder.ID, reminder.Text, reminder.MissedAt.In(s.location).Format("1月2日 15:04")))
		ids = append(ids, reminder.ID)
	}
	return brain.Briefing{Lines: lines, Delivered: func() {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		if err := s.store.MissTold(ctx, ids...); err != nil {
			s.logger.Warn("标记错过的提醒已告诉失败：之后会再说一次", "ids", ids, "err", err)
		}
	}}
}

func formatNext(next time.Time, location *time.Location) string {
	if next.IsZero() {
		return "不再到点"
	}
	return next.In(location).Format(time.RFC3339)
}

// storeTimeout：结果回调和情况说明里读写库最多等这么久（库在本机，正常几毫秒）；定值
const storeTimeout = 2 * time.Second

// caughtUpLate：到点后超过这么久才说出来的算补说（指标用）：正常等主人说完、终端播完一般不到一分钟；定值
const caughtUpLate = time.Minute

var (
	meter       = otel.Meter("github.com/bukahou/haro/reminders")
	nextDue     = telemetry.MustGauge(meter, "haro.reminders.next_due", "最近一次到点的提醒是什么时候（Unix 秒；0 = 没有在用的）", "s")
	spoken      = telemetry.MustCounter(meter, "haro.reminders.spoken", "到点说出来的提醒（chimed = 合成用不了改播提示音；caught_up = 晚了才补说）")
	missedCount = telemetry.MustCounter(meter, "haro.reminders.missed", "错过的提醒（到点后一直没有在线的终端，超过补说的时间）")
)
