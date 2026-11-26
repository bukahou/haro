package reminders

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// 重复规则用日历标准 RFC 5545 的 RRULE（例如 FREQ=DAILY、FREQ=WEEKLY;BYDAY=MO）：模型按主人的意思写，代码只解析和推算，
// 不自己理解「每周一」这样的话（P1）。解析和推算交给 rrule-go（包在本文件里，别处只用 recurrence）。

// minRecurrenceGap：重复的提醒两次之间至少隔这么久。挡住模型写错规则（例如 FREQ=SECONDLY）时一直响；
// 「每半小时提醒喝水」这样的用法照样能定。定值
const minRecurrenceGap = 5 * time.Minute

// gapSamples：检查两次之间的间隔时，从第一次起看这么多次（BYHOUR=8,9 这样一天几次的规则间隔不均匀，要多看几次）。
const gapSamples = 24

// recurrence 是解析好的重复规则：从 first（第一次到点）起按规则重复，规则里的钟点按 location 算。
type recurrence struct {
	rule *rrule.RRule
}

// parseRecurrence 解析模型交的重复规则：只收一行 RRULE（可以带「RRULE:」前缀），不许自带 DTSTART（第一次到点由
// at / after_seconds 定）；两次之间不能短于 minRecurrenceGap；first 本身要是一次到点（规则推不出 first 时拒绝，
// 例如说「每周一」却给了个周二的时间）。
func parseRecurrence(text string, first time.Time, location *time.Location) (recurrence, error) {
	text = strings.TrimSpace(text)
	if strings.Contains(text, "\n") || strings.Contains(strings.ToUpper(text), "DTSTART") {
		return recurrence{}, errors.New("rrule 只写一行重复规则，不带 DTSTART（第一次到点用 at 或 after_seconds 给）")
	}
	option, err := rrule.StrToROptionInLocation(text, location)
	if err != nil {
		return recurrence{}, fmt.Errorf("rrule 不是合法的 RFC 5545 重复规则：%w", err)
	}
	option.Dtstart = first.In(location)
	rule, err := rrule.NewRRule(*option)
	if err != nil {
		return recurrence{}, fmt.Errorf("rrule 不是合法的 RFC 5545 重复规则：%w", err)
	}
	if !rule.After(first, true).Equal(first.Truncate(time.Second)) {
		return recurrence{}, errors.New("第一次到点的时间不符合 rrule（例如规则是每周一，给的时间却是周二）：按规则给出第一次的时间")
	}
	previous := first.Truncate(time.Second)
	for range gapSamples {
		next := rule.After(previous, false)
		if next.IsZero() {
			break
		}
		if next.Sub(previous) < minRecurrenceGap {
			return recurrence{}, fmt.Errorf("rrule 两次之间太近（%v），至少隔 %v", next.Sub(previous), minRecurrenceGap)
		}
		previous = next
	}
	return recurrence{rule: rule}, nil
}

// after 是 t 之后（不含 t）的下一次到点；规则到头了（COUNT、UNTIL）返回 false。
func (r recurrence) after(t time.Time) (time.Time, bool) {
	next := r.rule.After(t, false)
	return next.UTC(), !next.IsZero()
}
