// Package calendar 是日历（第 18 段）：每个人的日程，语音和 App 共用的正本。重复规则（RFC 5545 RRULE）的展开、
// 「只改这一次 / 以后都改」只在这里算；日程要在终端响的提醒交给 internal/reminders 去说（每条提醒挂在日程上，
// 下一次到点问这里），要在手机响的由 App 按 Alerts 给的清单排本地通知。系统日历单向同步进来（只读）。
package calendar

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// 来源。
const (
	SourceHaro   = "haro"   // haro 自己的：能改
	SourceSystem = "system" // 从系统日历同步进来的：只读
)

// 上限（定值）。
const (
	MaxTitleRunes    = 200
	MaxTextRunes     = 2000 // 地点、备注
	MaxAlerts        = 5
	MaxAlertLead     = 7 * 24 * time.Hour
	maxOccurrences   = 1000 // 一次最多展开几次（表要有上限）
	minRecurrenceGap = 5 * time.Minute
)

// AlertChoices 是 App 新建 / 改日程时给主人选的提醒档位（少的在前）；语音定的提醒不限于这几个。
var AlertChoices = []time.Duration{0, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 24 * time.Hour}

// 错误（App 的接口按它们给状态）。
var (
	ErrNotFound = errors.New("没有这条日程")
	ErrReadOnly = errors.New("从系统日历同步进来的日程只读（要改去系统日历改）")
	ErrInvalid  = errors.New("日程不合规")
)

// Event 是一条日程。时间都是 UTC；全天的 Start 是那一天 00:00 UTC（只当日期用），End 是最后一天的第二天（不含）。
type Event struct {
	ID, Person                string
	Title, Location, Notes    string
	Start, End                time.Time
	AllDay                    bool
	Rule                      string      // RRULE 一行；空 = 一次
	Exdates                   []time.Time // 排除的那几次的开始时间
	Alerts                    []time.Duration
	AlertTerminal, AlertPhone bool
	Source, Calendar          string
	CalendarName, ExternalID  string
	Origin                    string // reminder = 语音定提醒时建的
	Language                  string
	CreatedAt, UpdatedAt      time.Time
	DeletedAt                 *time.Time
}

// Editable 判断能不能在 haro 里改。
func (e Event) Editable() bool { return e.Source == SourceHaro }

// Occurrence 是展开后的一次。
type Occurrence struct {
	Event      Event
	Start, End time.Time
}

// expander 展开一条日程的每一次。
type expander struct {
	event    Event
	set      *rrule.Set // nil = 一次
	duration time.Duration
}

// newExpander 按日程的重复规则准备展开；location 是重复规则里钟点的时区（全天的按 UTC 的日期算）。
func newExpander(event Event, location *time.Location) (expander, error) {
	ex := expander{event: event, duration: event.End.Sub(event.Start)}
	if event.Rule == "" {
		return ex, nil
	}
	if event.AllDay {
		location = time.UTC
	}
	rule, err := parseRule(event.Rule, event.Start, location)
	if err != nil {
		return expander{}, err
	}
	set := &rrule.Set{}
	set.RRule(rule)
	for _, at := range event.Exdates {
		set.ExDate(at.In(location))
	}
	ex.set = set
	return ex, nil
}

// parseRule 解析一行 RRULE（不许带 DTSTART：起点是日程的开始），起点本身要是一次，两次之间不能太近。
func parseRule(text string, start time.Time, location *time.Location) (*rrule.RRule, error) {
	text = strings.TrimSpace(text)
	if strings.Contains(text, "\n") || strings.Contains(strings.ToUpper(text), "DTSTART") {
		return nil, fmt.Errorf("%w：rrule 只写一行重复规则，不带 DTSTART", ErrInvalid)
	}
	option, err := rrule.StrToROptionInLocation(text, location)
	if err != nil {
		return nil, fmt.Errorf("%w：rrule 不是合法的 RFC 5545 重复规则：%v", ErrInvalid, err)
	}
	option.Dtstart = start.In(location)
	rule, err := rrule.NewRRule(*option)
	if err != nil {
		return nil, fmt.Errorf("%w：rrule 不是合法的 RFC 5545 重复规则：%v", ErrInvalid, err)
	}
	if !rule.After(start, true).Equal(start.Truncate(time.Second)) {
		return nil, fmt.Errorf("%w：开始时间不符合重复规则（例如规则是每周一，开始却是周二）", ErrInvalid)
	}
	if next := rule.After(start, false); !next.IsZero() && next.Sub(start) < minRecurrenceGap {
		return nil, fmt.Errorf("%w：重复得太密（两次之间至少 %v）", ErrInvalid, minRecurrenceGap)
	}
	return rule, nil
}

// between 是和 [from, to) 有交集的每一次（开始的先后，最多 maxOccurrences 次）。
func (x expander) between(from, to time.Time) []Occurrence {
	if x.set == nil {
		if x.event.Start.Before(to) && x.event.End.After(from) && !excluded(x.event, x.event.Start) {
			return []Occurrence{{Event: x.event, Start: x.event.Start, End: x.event.End}}
		}
		return nil
	}
	var out []Occurrence
	for _, start := range x.set.Between(from.Add(-x.duration), to, true) {
		end := start.Add(x.duration)
		if !end.After(from) || !start.Before(to) {
			continue
		}
		out = append(out, Occurrence{Event: x.event, Start: start.UTC(), End: end.UTC()})
		if len(out) >= maxOccurrences {
			break
		}
	}
	return out
}

// after 是 t 之后（不含）开始的下一次；没有了返回 false。
func (x expander) after(t time.Time) (time.Time, bool) {
	if x.set == nil {
		if x.event.Start.After(t) && !excluded(x.event, x.event.Start) {
			return x.event.Start, true
		}
		return time.Time{}, false
	}
	next := x.set.After(t, false)
	return next.UTC(), !next.IsZero()
}

func excluded(event Event, start time.Time) bool {
	for _, at := range event.Exdates {
		if at.Equal(start) {
			return true
		}
	}
	return false
}

// untilBefore 把重复规则截到 at 之前（「以后都改」时原来那条到这一次之前为止）。
func untilBefore(rule string, at time.Time, location *time.Location) (string, error) {
	option, err := rrule.StrToROptionInLocation(rule, location)
	if err != nil {
		return "", err
	}
	option.Count = 0
	option.Until = at.Add(-time.Second).In(location)
	text := option.RRuleString()
	return text, nil
}
