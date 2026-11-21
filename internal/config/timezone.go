package config

import (
	"os"
	"time"
)

// TimeZone 是进程实际用的本地时区：「今天」「这个月」（每日、每月的费用上限什么时候翻篇）、告诉模型的现在时间、
// 记事显示的时间都按它算（存储一律是 UTC）。时区不单独做配置项：用标准的环境变量 TZ，不设时按系统的时区
// （容器里一般是 UTC）。启动时写进日志，时区不对一眼看得出来。
type TimeZone struct {
	Name   string // 时区的缩写，例如 JST、UTC
	Offset string // 和 UTC 差多少，例如 +09:00
	TZ     string // 环境变量 TZ 的值；空 = 没设（按系统的时区）
}

// LocalTimeZone 是 now 所在的时区（启动时传 time.Now()，即进程的本地时区）。
func LocalTimeZone(now time.Time) TimeZone {
	name, _ := now.Zone()
	return TimeZone{Name: name, Offset: now.Format("-07:00"), TZ: os.Getenv("TZ")}
}
