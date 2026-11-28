package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/calendar"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/reminders"
)

// startReminders 打开提醒的存储（和记事本同一个库文件：HARO_NOTES_DB）、日历（同一个库文件），把原来的提醒收编成日程，
// 再起调度循环（挂在日程上的提醒下一次到点问日历：先接上日历再跑）。到点的提醒交给 notices
// （这个人最近说话的那台终端）说出来；有终端连上时叫醒调度（等着终端的提醒可以补说了）。
// 返回的 stop 先等调度循环退出，再关库。
func startReminders(ctx context.Context, settings config.ServerSettings, notices *brain.NoticeHub,
	logger *slog.Logger) (*reminders.Scheduler, *calendar.Service, func() error, error) {
	store, err := reminders.OpenSQLite(ctx, settings.NotesDB)
	if err != nil {
		return nil, nil, nil, err
	}
	scheduler := reminders.NewScheduler(store, notices, reminders.SchedulerOptions{}, logger)
	phrases, err := brain.LoadPhrases(settings.PhrasesFile)
	if err != nil {
		return nil, nil, nil, err
	}
	events, err := calendar.OpenStore(ctx, settings.NotesDB)
	if err != nil {
		return nil, nil, nil, err
	}
	calendarService := calendar.NewService(events, calendar.Options{Scheduler: scheduler, Phrases: phrases.Say,
		AlertKey: brain.PhraseCalendarAlertAhead, Location: scheduler.Location(), Logger: logger})
	scheduler.UseEvents(calendarService)
	if err := calendarService.Adopt(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("把原来的提醒收编成日程: %w", err)
	}
	notices.OnTerminalJoined(scheduler.Poke)
	running := make(chan struct{})
	go func() {
		defer close(running)
		scheduler.Run(ctx) // ctx 随退出信号结束
	}()
	logger.Info("提醒已开启", "catch_up_window", reminders.CatchUpWindow, "max_active_per_person", reminders.MaxActivePerPerson,
		"zone", scheduler.Location().String())
	stop := func() error {
		select {
		case <-running:
		case <-time.After(shutdownReminders):
			logger.Warn("提醒的调度没在期限内停下：照样关库", "timeout", shutdownReminders)
		}
		return errors.Join(store.Close(), events.Close())
	}
	return scheduler, calendarService, stop, nil
}
