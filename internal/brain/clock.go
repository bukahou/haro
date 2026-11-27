package brain

import (
	"fmt"
	"time"
)

// ClockBriefer 每轮告诉模型现在的日期和时间：不说的话，模型联网搜索时会用训练时的年份
// （实测：搜索词里写的是两年前），回答「今年」「下周」也会算错。一行，约 20 token。
type ClockBriefer struct {
	Now func() time.Time // 测试用；nil = time.Now
}

var weekdayNames = []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

// WeekdayName 是 t 那天是星期几（中文，例如「星期三」）：星期的写法只在这里定义一次。
func WeekdayName(t time.Time) string { return weekdayNames[t.Weekday()] }

// DateText 是告诉模型的日期（例如「2026年11月17日（星期二）」）：每轮的情况说明、后台调查的任务都用这一种写法。
func DateText(t time.Time) string {
	return t.Format("2006年1月2日") + "（" + WeekdayName(t) + "）"
}

// ElapsedText 是一段时间告诉模型的说法（例如「3 小时 12 分钟」「2 天 5 小时」）。
func ElapsedText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "不到 1 分钟"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	case d < 24*time.Hour:
		hours, minutes := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("%d 小时", hours)
		}
		return fmt.Sprintf("%d 小时 %d 分钟", hours, minutes)
	}
	days, hours := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
	if hours == 0 {
		return fmt.Sprintf("%d 天", days)
	}
	return fmt.Sprintf("%d 天 %d 小时", days, hours)
}

// Brief 实现 Briefer。
func (c ClockBriefer) Brief(string) Briefing {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	return Briefing{Lines: []string{fmt.Sprintf("现在是 %s%s。", DateText(now), now.Format("15:04"))}}
}
