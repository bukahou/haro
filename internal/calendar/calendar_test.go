package calendar

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/reminders"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type silentAnnouncer struct{}

func (silentAnnouncer) Announce(brain.Announcement) bool { return false }

type fixture struct {
	calendar  *Service
	scheduler *reminders.Scheduler
	now       time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	reminderStore, err := reminders.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reminderStore.Close() })
	store, err := OpenStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &fixture{now: time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return f.now }
	f.scheduler = reminders.NewScheduler(reminderStore, silentAnnouncer{}, reminders.SchedulerOptions{Location: time.UTC, Now: clock}, quiet)
	f.calendar = NewService(store, Options{Scheduler: f.scheduler, Location: time.UTC, Now: clock, Logger: quiet, AlertKey: "ahead",
		Phrases: func(_, _ string, values map[string]string) string {
			return values["title"] + " 还有 " + values["minutes"] + " 分钟"
		}})
	f.scheduler.UseEvents(f.calendar)
	return f
}

// 每天的日程、提前 10 分钟：展开出每一天；终端那边挂了一条提醒（下一次到点、到点说的话）；之后的到点问日历。
func TestCreateExpandAndArm(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	start := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)
	event, err := f.calendar.Create(ctx, "主人", Input{Title: "吃药", Start: start, End: start.Add(15 * time.Minute), Rule: "FREQ=DAILY",
		Alerts: []time.Duration{10 * time.Minute}, AlertTerminal: true, AlertPhone: true})
	if err != nil {
		t.Fatal(err)
	}
	occurrences, err := f.calendar.Occurrences(ctx, "主人", start, start.AddDate(0, 0, 3))
	if err != nil || len(occurrences) != 3 {
		t.Fatalf("三天：%d %v", len(occurrences), err)
	}
	armed, err := f.scheduler.EventReminders(ctx, event.ID)
	if err != nil || len(armed) != 1 || !armed[0].NextAt.Equal(start.Add(-10*time.Minute)) || armed[0].Text != "吃药 还有 10 分钟" {
		t.Fatalf("终端那边的提醒：%+v %v", armed, err)
	}
	next, ok, err := f.calendar.NextAlert(ctx, event.ID, 10*time.Minute, start)
	if err != nil || !ok || !next.Equal(start.AddDate(0, 0, 1).Add(-10*time.Minute)) {
		t.Errorf("下一次到点：%v %v %v", next, ok, err)
	}
	alerts, err := f.calendar.PhoneAlerts(ctx, "主人", f.now.Add(48*time.Hour))
	if err != nil || len(alerts) != 2 || !alerts[0].At.Equal(start.Add(-10*time.Minute)) {
		t.Errorf("手机的提醒：%+v %v", alerts, err)
	}
	if _, err := f.calendar.Create(ctx, "主人", Input{Title: "错", Start: start, End: start, Rule: "FREQ=WEEKLY;BYDAY=MO"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("开始不符合规则（12/1 是周二）：%v", err)
	}
}

// 改重复的：只改这一次（原来的排除那一次、另建一条）；以后都改（原来的截到那一次之前）；删整个系列 = 终端的提醒取消。
func TestEditScopes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	start := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)
	base := Input{Title: "晨跑", Start: start, End: start.Add(time.Hour), Rule: "FREQ=DAILY", Alerts: []time.Duration{0}, AlertTerminal: true}
	event, err := f.calendar.Create(ctx, "主人", base)
	if err != nil {
		t.Fatal(err)
	}
	second := start.AddDate(0, 0, 1)
	moved := base
	moved.Start, moved.End = second.Add(2*time.Hour), second.Add(3*time.Hour)
	if _, err := f.calendar.Update(ctx, "主人", event.ID, moved, Scope{Kind: ScopeThis, At: second}); err != nil {
		t.Fatal(err)
	}
	occurrences, _ := f.calendar.Occurrences(ctx, "主人", start, start.AddDate(0, 0, 3))
	if len(occurrences) != 3 || !occurrences[1].Start.Equal(second.Add(2*time.Hour)) || occurrences[1].Event.Rule != "" {
		t.Fatalf("只改第二天：%+v", occurrences)
	}
	third := start.AddDate(0, 0, 2)
	later := base
	later.Title, later.Start, later.End = "夜跑", third.Add(10*time.Hour), third.Add(11*time.Hour)
	if _, err := f.calendar.Update(ctx, "主人", event.ID, later, Scope{Kind: ScopeFuture, At: third}); err != nil {
		t.Fatal(err)
	}
	occurrences, _ = f.calendar.Occurrences(ctx, "主人", start, start.AddDate(0, 0, 4))
	titles := []string{}
	for _, occurrence := range occurrences {
		titles = append(titles, occurrence.Event.Title)
	}
	if len(occurrences) != 4 || titles[0] != "晨跑" || titles[2] != "夜跑" || titles[3] != "夜跑" {
		t.Fatalf("第三天起改成夜跑：%v", titles)
	}
	if _, err := f.calendar.Update(ctx, "主人", event.ID, moved, Scope{Kind: ScopeThis, At: second.Add(time.Minute)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("那个时间没有一次：%v", err)
	}
	if err := f.calendar.Delete(ctx, "主人", event.ID, Scope{Kind: ScopeAll}); err != nil {
		t.Fatal(err)
	}
	if armed, _ := f.scheduler.EventReminders(ctx, event.ID); len(armed) != 0 {
		t.Errorf("删了：终端的提醒取消：%+v", armed)
	}
	if _, err := f.calendar.Get(ctx, "别人", event.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("别人的：%v", err)
	}
}

// 系统日历同步：增、改、这次没交来的删掉；同步进来的只读、不在终端响、不在手机响（系统日历自己会提醒）。
func TestSync(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	start := time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC)
	first := []SyncedEvent{{ExternalID: "a", Title: "公司年会", Start: start, End: start.AddDate(0, 0, 1), AllDay: true},
		{ExternalID: "b", Title: "牙医", Start: start.Add(15 * time.Hour), End: start.Add(16 * time.Hour)}}
	if result, err := f.calendar.Sync(ctx, "主人", "icloud-1", "家里", first); err != nil || result.Added != 2 {
		t.Fatalf("第一次：%+v %v", result, err)
	}
	second := []SyncedEvent{{ExternalID: "a", Title: "公司年会（改了）", Start: start, End: start.AddDate(0, 0, 1), AllDay: true}}
	if result, err := f.calendar.Sync(ctx, "主人", "icloud-1", "家里", second); err != nil || result.Updated != 1 || result.Removed != 1 {
		t.Fatalf("第二次：%+v %v", result, err)
	}
	occurrences, _ := f.calendar.Occurrences(ctx, "主人", start, start.AddDate(0, 0, 1))
	if len(occurrences) != 1 || occurrences[0].Event.Title != "公司年会（改了）" || occurrences[0].Event.Editable() {
		t.Fatalf("同步进来的：%+v", occurrences)
	}
	if _, err := f.calendar.Update(ctx, "主人", occurrences[0].Event.ID, Input{Title: "x", Start: start, End: start}, Scope{}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("只读：%v", err)
	}
	if syncs, _ := f.calendar.Syncs(ctx, "主人"); len(syncs) != 1 || syncs[0].Name != "家里" {
		t.Errorf("上次同步：%+v", syncs)
	}
}

// 原来的提醒收编成日程（规则照搬、挂上 event_id、之后下一次问日历）；语音定提醒同时建日程，取消提醒时日程跟着删，恢复时跟着恢复。
func TestRemindersBecomeEvents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old, err := f.scheduler.Set(ctx, "主人", reminders.Request{Text: "该喝水了", At: f.now.Add(time.Hour), Rule: "FREQ=DAILY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.calendar.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	adopted, err := f.scheduler.EventReminders(ctx, mustEventID(t, f, old.ID))
	if err != nil || len(adopted) != 1 || adopted[0].Rule != "" {
		t.Fatalf("收编：%+v %v", adopted, err)
	}
	if occurrences, _ := f.calendar.Occurrences(ctx, "主人", f.now, f.now.AddDate(0, 0, 2)); len(occurrences) != 2 || occurrences[0].Event.Title != "该喝水了" {
		t.Errorf("日历里看得到：%+v", occurrences)
	}
	if err := f.calendar.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	desk := ReminderDesk{Calendar: f.calendar, Scheduler: f.scheduler}
	set, err := desk.Set(ctx, "主人", reminders.Request{Text: "开会", After: 2 * time.Hour})
	if err != nil || set.EventID == "" {
		t.Fatalf("语音定提醒：%+v %v", set, err)
	}
	if _, err := desk.Cancel(ctx, "主人", set.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.calendar.Get(ctx, "主人", set.EventID); !errors.Is(err, ErrNotFound) {
		t.Errorf("取消提醒：日程跟着删：%v", err)
	}
	if _, err := desk.Restore(ctx, "主人", set.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.calendar.Get(ctx, "主人", set.EventID); err != nil {
		t.Errorf("恢复提醒：日程跟着恢复：%v", err)
	}
}

// mustEventID 是一条提醒挂在哪条日程上。
func mustEventID(t *testing.T, f *fixture, id int64) string {
	t.Helper()
	listing, err := f.scheduler.List(context.Background(), "主人", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, reminder := range listing.Active {
		if reminder.ID == id {
			return reminder.EventID
		}
	}
	t.Fatalf("没找到提醒 %d", id)
	return ""
}
