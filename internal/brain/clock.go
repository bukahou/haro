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

var clockWeekdays = []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

// Brief 实现 Briefer。
func (c ClockBriefer) Brief(string) Briefing {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	return Briefing{Lines: []string{fmt.Sprintf("现在是 %s（%s）%s。", now.Format("2006年1月2日"), clockWeekdays[now.Weekday()], now.Format("15:04"))}}
}
