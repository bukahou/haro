package reminders

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// fakeAnnouncer 记下交来的话；online 为 false 时像没有在线的终端那样交不出去。结果由测试调 Done 给。
type fakeAnnouncer struct {
	mu     sync.Mutex
	online bool
	got    []brain.Announcement
}

func (f *fakeAnnouncer) Announce(a brain.Announcement) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.online {
		return false
	}
	f.got = append(f.got, a)
	return true
}

func (f *fakeAnnouncer) taken() []brain.Announcement {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.got
	f.got = nil
	return got
}

// clock 是测试里可以拨的时钟。
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var jst = time.FixedZone("JST", 9*3600)

func newScheduler(t *testing.T) (*Scheduler, *fakeAnnouncer, *clock) {
	t.Helper()
	store, err := OpenSQLite(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	announcer := &fakeAnnouncer{online: true}
	now := &clock{now: time.Date(2026, 11, 27, 7, 0, 0, 0, jst)}
	scheduler := NewScheduler(store, announcer, SchedulerOptions{Location: jst, Now: now.Now},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return scheduler, announcer, now
}

func TestSetValidates(t *testing.T) {
	s, _, now := newScheduler(t)
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		request Request
		want    string
	}{
		{"没有要说的话", Request{After: time.Minute}, "text"},
		{"太长", Request{Text: strings.Repeat("喝", MaxTextRunes+1), After: time.Minute}, "太长"},
		{"时间两个都给", Request{Text: "喝水", After: time.Minute, At: now.Now().Add(time.Hour)}, "恰好给一个"},
		{"时间都没给", Request{Text: "喝水"}, "恰好给一个"},
		{"已经过了", Request{Text: "喝水", At: now.Now().Add(-time.Minute)}, "已经过了"},
		{"太远", Request{Text: "喝水", After: MaxAhead + time.Hour}, "最远"},
		{"规则不对", Request{Text: "喝水", After: time.Hour, Rule: "每天"}, "rrule"},
	} {
		if _, err := s.Set(ctx, "主人", c.request); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应拒绝（含「%s」），得到 %v", c.name, c.want, err)
		}
	}
	for i := range MaxActivePerPerson {
		if _, err := s.Set(ctx, "主人", Request{Text: "喝水", After: time.Duration(i+1) * time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Set(ctx, "主人", Request{Text: "喝水", After: time.Hour}); err == nil || !strings.Contains(err.Error(), "最多") {
		t.Errorf("在用的满了应拒绝：%v", err)
	}
	if _, err := s.Set(ctx, "老大", Request{Text: "写作业", After: time.Hour}); err != nil {
		t.Errorf("上限按人算：%v", err)
	}
}

// 到点交给开口的一方；说出来以后一次性的不再到点；结果出来之前不重复交。
func TestDueReminderIsAnnouncedOnce(t *testing.T) {
	s, announcer, now := newScheduler(t)
	ctx := context.Background()
	reminder, err := s.Set(ctx, "主人", Request{Text: "该喝水了。", After: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if sleep := s.handleDue(ctx); sleep != 10*time.Minute || len(announcer.taken()) != 0 {
		t.Fatalf("还没到点：睡到到点（%v）", sleep)
	}
	now.advance(10 * time.Minute)
	s.handleDue(ctx)
	got := announcer.taken()
	if len(got) != 1 || got[0].Text != "该喝水了。" || got[0].Person != "主人" || got[0].Reason != realtimeproto.UnpromptedReminder ||
		!strings.Contains(got[0].Note, "#1") {
		t.Fatalf("到点应交出去：%+v", got)
	}
	s.handleDue(ctx)
	if again := announcer.taken(); len(again) != 0 {
		t.Fatalf("结果出来之前不重复交：%+v", again)
	}
	got[0].Done(brain.AnnouncementSpoken)
	s.handleDue(ctx)
	if again := announcer.taken(); len(again) != 0 {
		t.Fatalf("说过的一次性提醒不再到点：%+v", again)
	}
	listing, _ := s.List(ctx, "主人", false)
	if len(listing.Active) != 0 {
		t.Errorf("说过以后不在在用的里面：%+v", listing.Active)
	}
	stored, _ := s.store.Get(ctx, "主人", reminder.ID)
	if stored.SpokenAt.IsZero() {
		t.Error("应记下说出来的时间")
	}
}

// 重复的提醒说过以后按规则算下一次。
func TestRepeatingReminderMovesToNextOccurrence(t *testing.T) {
	s, announcer, now := newScheduler(t)
	ctx := context.Background()
	first := time.Date(2026, 11, 27, 8, 0, 0, 0, jst)
	if _, err := s.Set(ctx, "主人", Request{Text: "该吃药了。", At: first, Rule: "FREQ=DAILY"}); err != nil {
		t.Fatal(err)
	}
	now.advance(time.Hour) // 8:00
	s.handleDue(ctx)
	got := announcer.taken()
	if len(got) != 1 {
		t.Fatal("到点应交出去")
	}
	got[0].Done(brain.AnnouncementSpoken)
	listing, _ := s.List(ctx, "主人", false)
	if len(listing.Active) != 1 || !listing.Active[0].NextAt.Equal(first.Add(24*time.Hour)) {
		t.Errorf("下一次应是明天 8 点：%+v", listing.Active)
	}
}

// 到点时没有在线的终端：留着，CatchUpWindow 以内终端连上就补说；超过就记成错过，下次开口时告诉主人。
func TestUndeliveredReminderCatchesUpOrIsMissed(t *testing.T) {
	s, announcer, now := newScheduler(t)
	ctx := context.Background()
	announcer.online = false
	if _, err := s.Set(ctx, "主人", Request{Text: "该喝水了。", After: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now.advance(time.Minute)
	if sleep := s.handleDue(ctx); sleep != defaultRetry {
		t.Errorf("交不出去：隔 %v 再试，得到 %v", defaultRetry, sleep)
	}
	now.advance(10 * time.Minute)
	announcer.online = true // 终端连上了
	s.handleDue(ctx)
	got := announcer.taken()
	if len(got) != 1 {
		t.Fatal("还在补说的时间以内：应补说")
	}
	got[0].Done(brain.AnnouncementSpoken)

	announcer.online = false
	if _, err := s.Set(ctx, "主人", Request{Text: "该吃药了。", After: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now.advance(time.Minute)
	s.handleDue(ctx)
	now.advance(CatchUpWindow + time.Minute)
	announcer.online = true
	s.handleDue(ctx)
	if late := announcer.taken(); len(late) != 0 {
		t.Fatalf("超过补说的时间就不说了：%+v", late)
	}
	briefing := s.Brief("主人")
	if len(briefing.Lines) != 1 || !strings.Contains(briefing.Lines[0], "该吃药了") || briefing.Delivered == nil {
		t.Fatalf("错过的应附在情况说明里：%+v", briefing)
	}
	if other := s.Brief("老大"); len(other.Lines) != 0 {
		t.Errorf("只告诉定的那个人：%+v", other)
	}
	briefing.Interrupted() // 附上的那一轮被打断：再附上一次，说明上次被打断了
	again := s.Brief("主人")
	if len(again.Lines) != 1 || !strings.Contains(again.Lines[0], brain.CutNote) {
		t.Fatalf("被打断后再附上一次：%+v", again)
	}
	again.Delivered()
	if again := s.Brief("主人"); len(again.Lines) != 0 {
		t.Errorf("听完就算告诉了：%+v", again)
	}
}

// 附上「错过了」的那一轮一直被打断：再附上一次以后就当告诉过了。
func TestMissedBriefingGivesUpAfterRepeatedCuts(t *testing.T) {
	s, announcer, now := newScheduler(t)
	ctx := context.Background()
	announcer.online = false
	if _, err := s.Set(ctx, "主人", Request{Text: "该吃药了。", After: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now.advance(CatchUpWindow + 2*time.Minute)
	s.handleDue(ctx)
	s.Brief("主人").Interrupted()
	s.Brief("主人").Interrupted()
	if left := s.Brief("主人"); len(left.Lines) != 0 {
		t.Fatalf("被打断两次就不再附上：%+v", left)
	}
	if missed, _ := s.store.Unannounced(ctx, "主人"); len(missed) != 0 {
		t.Errorf("当告诉过了：%+v", missed)
	}
}

// 开口没能说成（会话关了）：留着，之后再试。
func TestDroppedAnnouncementIsRetried(t *testing.T) {
	s, announcer, now := newScheduler(t)
	ctx := context.Background()
	if _, err := s.Set(ctx, "主人", Request{Text: "该喝水了。", After: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now.advance(time.Minute)
	s.handleDue(ctx)
	announcer.taken()[0].Done(brain.AnnouncementDropped)
	s.handleDue(ctx)
	if again := announcer.taken(); len(again) != 1 {
		t.Fatalf("没能说成的应再交一次：%+v", again)
	}
}

func TestCancelAndRestore(t *testing.T) {
	s, announcer, now := newScheduler(t)
	ctx := context.Background()
	once, _ := s.Set(ctx, "主人", Request{Text: "该喝水了。", After: 10 * time.Minute})
	daily, _ := s.Set(ctx, "主人", Request{Text: "该吃药了。", At: time.Date(2026, 11, 27, 8, 0, 0, 0, jst), Rule: "FREQ=DAILY"})
	if _, err := s.Cancel(ctx, "老大", once.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("不能取消别人的：%v", err)
	}
	for _, id := range []int64{once.ID, daily.ID} {
		if _, err := s.Cancel(ctx, "主人", id); err != nil {
			t.Fatal(err)
		}
	}
	now.advance(10 * time.Minute)
	s.handleDue(ctx)
	if got := announcer.taken(); len(got) != 0 {
		t.Fatalf("取消了的不说：%+v", got)
	}
	listing, _ := s.List(ctx, "主人", true)
	if len(listing.Active) != 0 || len(listing.Cancelled) != 2 {
		t.Errorf("列出最近取消的：%+v", listing)
	}
	if _, err := s.Restore(ctx, "主人", once.ID); err == nil || !strings.Contains(err.Error(), "已经过了") {
		t.Errorf("一次性的时间过了不能恢复：%v", err)
	}
	now.advance(2 * time.Hour) // 9:10：今天 8 点那次已经过了
	restored, err := s.Restore(ctx, "主人", daily.ID)
	if err != nil || !restored.NextAt.Equal(time.Date(2026, 11, 28, 8, 0, 0, 0, jst)) {
		t.Fatalf("重复的从现在起接着算（明天 8 点）：%v %+v", err, restored)
	}
	if _, err := s.Restore(ctx, "主人", daily.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("没取消的不能恢复：%v", err)
	}
}

// Run：有变化时（Poke）马上重新看一遍；ctx 结束就退出。
func TestRunAnnouncesWhenPoked(t *testing.T) {
	s, announcer, now := newScheduler(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	if _, err := s.Set(ctx, "主人", Request{Text: "该喝水了。", After: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now.advance(time.Minute)
	s.Poke()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := announcer.taken(); len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Poke 之后应马上交出去")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 结束后应退出")
	}
}
