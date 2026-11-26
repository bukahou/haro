package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/reminders"
)

// nobody 是没有在线终端的开口的一方（工具的测试不到点）。
type nobody struct{}

func (nobody) Announce(brain.Announcement) bool { return false }

func newReminderBox(t *testing.T) *Toolbox {
	t.Helper()
	ctx := context.Background()
	store, err := reminders.OpenSQLite(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 11, 27, 7, 0, 0, 0, time.FixedZone("JST", 9*3600))
	scheduler := reminders.NewScheduler(store, nobody{}, reminders.SchedulerOptions{Location: now.Location(),
		Now: func() time.Time { return now }}, quietLogger)
	box, err := NewToolbox(ctx, quietLogger, ReminderTools(scheduler, quietLogger))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(box.Close)
	return box
}

// 主人（声纹认出）在说话的一轮。
func ownerTurn() context.Context {
	return brain.WithVoiceVerified(brain.WithSpeaker(context.Background(), "主人"), true)
}

// 定、查：只有声纹认出的这句能定；时间按服务端的时区写给模型，带上离到点还有多少秒；只看得到自己的。
func TestReminderToolsSetAndList(t *testing.T) {
	box := newReminderBox(t)
	set := map[string]any{"text": "该喝水了。", "after_seconds": 600}
	unverified := brain.WithSpeaker(context.Background(), "主人") // 声纹没算出来、沿用上一句的人：不算本人
	if _, err := callJSONWith[reminderOutput](t, unverified, box, SetReminderTool, set); err == nil ||
		!strings.Contains(err.Error(), "没认出") {
		t.Fatalf("声纹没认出时不能定：%v", err)
	}
	created, err := callJSONWith[reminderOutput](t, ownerTurn(), box, SetReminderTool, set)
	if err != nil {
		t.Fatal(err)
	}
	if created.Reminder.NextAt != "2026-11-27T07:10:00+09:00" || created.Reminder.InSeconds == nil || *created.Reminder.InSeconds != 600 {
		t.Errorf("时间按服务端时区写、带还有多少秒：%+v", created.Reminder)
	}
	if _, err := callJSONWith[reminderOutput](t, ownerTurn(), box, SetReminderTool,
		map[string]any{"text": "该吃药了。", "at": "2026-11-28T08:00:00+09:00", "rrule": "FREQ=DAILY"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callJSONWith[reminderOutput](t, ownerTurn(), box, SetReminderTool,
		map[string]any{"text": "该吃药了。", "at": "明天早上八点"}); err == nil || !strings.Contains(err.Error(), "RFC 3339") {
		t.Errorf("at 不是 RFC 3339 时说清楚：%v", err)
	}
	listed, err := callJSONWith[reminderListOutput](t, ownerTurn(), box, ListRemindersTool, map[string]any{})
	if err != nil || len(listed.Reminders) != 2 || listed.Reminders[0].Text != "该喝水了。" || listed.Reminders[1].RRule != "FREQ=DAILY" {
		t.Fatalf("按下一次到点排：%+v %v", listed, err)
	}
	other := brain.WithVoiceVerified(brain.WithSpeaker(context.Background(), "老大"), true)
	if theirs, _ := callJSONWith[reminderListOutput](t, other, box, ListRemindersTool, map[string]any{}); len(theirs.Reminders) != 0 {
		t.Errorf("只看得到自己的：%+v", theirs)
	}
}

// 取消会破坏：工具箱自己守门，没有主人的确认不执行；确认后取消，没有的编号写进 not_found；恢复要本人的声音。
func TestReminderToolsCancelAndRestore(t *testing.T) {
	box := newReminderBox(t)
	created, err := callJSONWith[reminderOutput](t, ownerTurn(), box, SetReminderTool, map[string]any{"text": "该喝水了。", "after_seconds": 600})
	if err != nil {
		t.Fatal(err)
	}
	cancel := map[string]any{"ids": []any{created.Reminder.ID, 99}}
	if _, err := box.Call(ownerTurn(), brain.ToolCall{Name: CancelReminderTool, Arguments: cancel}); !errors.Is(err, brain.ErrNotConfirmed) {
		t.Fatalf("没确认不该取消：%v", err)
	}
	confirmed := brain.WithSpeaker(confirmedFor(CancelReminderTool), "主人")
	cancelled, err := callJSONWith[cancelRemindersOutput](t, confirmed, box, CancelReminderTool, cancel)
	if err != nil || len(cancelled.Cancelled) != 1 || len(cancelled.NotFound) != 1 || cancelled.NotFound[0] != 99 {
		t.Fatalf("取消：%+v %v", cancelled, err)
	}
	listed, _ := callJSONWith[reminderListOutput](t, ownerTurn(), box, ListRemindersTool, map[string]any{"include_cancelled": true})
	if len(listed.Reminders) != 0 || len(listed.Cancelled) != 1 || listed.Cancelled[0].CancelledAt == "" {
		t.Errorf("取消后列在最近取消的里：%+v", listed)
	}
	restore := map[string]any{"id": created.Reminder.ID}
	if _, err := callJSONWith[reminderOutput](t, brain.WithSpeaker(context.Background(), "主人"), box, RestoreReminderTool, restore); err == nil {
		t.Error("声纹没认出时不能恢复")
	}
	restored, err := callJSONWith[reminderOutput](t, ownerTurn(), box, RestoreReminderTool, restore)
	if err != nil || restored.Reminder.CancelledAt != "" {
		t.Fatalf("恢复：%+v %v", restored, err)
	}
	if _, err := callJSONWith[reminderOutput](t, ownerTurn(), box, RestoreReminderTool, restore); err == nil ||
		!strings.Contains(err.Error(), "没有这个编号") {
		t.Errorf("没取消的恢复不了：%v", err)
	}
}

// 取消标成会破坏（过确认闸），定、恢复不用确认，查是只读。
func TestReminderToolAnnotations(t *testing.T) {
	needs := confirmations(newReminderBox(t))
	for name, want := range map[string]bool{SetReminderTool: false, ListRemindersTool: false, CancelReminderTool: true,
		RestoreReminderTool: false} {
		if got, ok := needs[name]; !ok || got != want {
			t.Errorf("%s 要不要确认：%v（有没有这个工具：%v），应为 %v", name, got, ok, want)
		}
	}
}
