package calendar

import (
	"context"
	"time"

	"github.com/bukahou/haro/internal/reminders"
)

// ReminderScheduler 是语音的提醒工具用到的调度（reminders.Scheduler 实现）。
type ReminderScheduler interface {
	Scheduler
	Set(ctx context.Context, person string, request reminders.Request) (reminders.Reminder, error)
	List(ctx context.Context, person string, withCancelled bool) (reminders.Listing, error)
	Cancel(ctx context.Context, person string, id int64) (reminders.Reminder, error)
	Restore(ctx context.Context, person string, id int64) (reminders.Reminder, error)
	Location() *time.Location
	Now() time.Time
}

// ReminderDesk 是语音的提醒工具（tools.ReminderDesk）接在日历上：定提醒时同时建一条日程（App 里看得到），
// 取消、恢复语音定的提醒时那条日程跟着删、恢复。校验、到点、补说照旧由提醒的调度做。
type ReminderDesk struct {
	Calendar  *Service
	Scheduler ReminderScheduler
}

// Set 定一条提醒：调度校验、存下，再建日程、把提醒挂上去。
func (d ReminderDesk) Set(ctx context.Context, person string, request reminders.Request) (reminders.Reminder, error) {
	reminder, err := d.Scheduler.Set(ctx, person, request)
	if err != nil {
		return reminders.Reminder{}, err
	}
	event, err := d.Calendar.eventForReminder(person, reminder.Text, reminder.StartsAt, reminder.Rule)
	if err != nil {
		return reminder, err
	}
	if err := d.Calendar.store.put(ctx, event); err != nil {
		return reminder, err
	}
	if err := d.Scheduler.Adopt(ctx, reminder.ID, event.ID); err != nil {
		return reminder, err
	}
	reminder.EventID, reminder.Rule = event.ID, ""
	return reminder, nil
}

// List 实现 tools.ReminderDesk。
func (d ReminderDesk) List(ctx context.Context, person string, withCancelled bool) (reminders.Listing, error) {
	return d.Scheduler.List(ctx, person, withCancelled)
}

// Cancel 取消一条提醒；是语音定提醒时建的日程就跟着删。
func (d ReminderDesk) Cancel(ctx context.Context, person string, id int64) (reminders.Reminder, error) {
	reminder, err := d.Scheduler.Cancel(ctx, person, id)
	if err != nil || reminder.EventID == "" {
		return reminder, err
	}
	d.followEvent(ctx, person, reminder.EventID, true)
	return reminder, nil
}

// Restore 恢复一条提醒；它的日程删了就跟着恢复。
func (d ReminderDesk) Restore(ctx context.Context, person string, id int64) (reminders.Reminder, error) {
	reminder, err := d.Scheduler.Restore(ctx, person, id)
	if err != nil || reminder.EventID == "" {
		return reminder, err
	}
	d.followEvent(ctx, person, reminder.EventID, false)
	return reminder, nil
}

// followEvent 让语音定提醒时建的那条日程跟着提醒删（deleted = true）或恢复。出错只记日志：提醒本身已经取消 / 恢复了。
func (d ReminderDesk) followEvent(ctx context.Context, person, eventID string, deleted bool) {
	event, err := d.Calendar.store.get(ctx, eventID)
	if err != nil || event.Person != person || event.Origin != OriginReminder {
		return
	}
	now := d.Calendar.now().UTC().Truncate(time.Second)
	if deleted {
		event.DeletedAt = &now
	} else {
		event.DeletedAt = nil
	}
	event.UpdatedAt = now
	if err := d.Calendar.store.put(ctx, event); err != nil {
		d.Calendar.logger.Warn("提醒的日程没能跟着改", "event", eventID, "deleted", deleted, "err", err)
	}
}

// Location 实现 tools.ReminderDesk。
func (d ReminderDesk) Location() *time.Location { return d.Scheduler.Location() }

// Now 实现 tools.ReminderDesk。
func (d ReminderDesk) Now() time.Time { return d.Scheduler.Now() }
