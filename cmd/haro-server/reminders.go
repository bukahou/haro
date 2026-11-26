package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/reminders"
)

// startReminders 打开提醒的存储（和记事本同一个库文件：HARO_NOTES_DB）、起调度循环。到点的提醒交给 notices
// （这个人最近说话的那台终端）说出来；有终端连上时叫醒调度（等着终端的提醒可以补说了）。
// 返回的 stop 先等调度循环退出，再关库。
func startReminders(ctx context.Context, settings config.ServerSettings, notices *brain.NoticeHub,
	logger *slog.Logger) (*reminders.Scheduler, func() error, error) {
	store, err := reminders.OpenSQLite(ctx, settings.NotesDB)
	if err != nil {
		return nil, nil, err
	}
	scheduler := reminders.NewScheduler(store, notices, reminders.SchedulerOptions{}, logger)
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
		return store.Close()
	}
	return scheduler, stop, nil
}
