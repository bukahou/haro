// Package holidays 告诉模型今天、明天是不是节假日（实现 brain.Briefer 要的一行行说明，由装配处接上）：
// 「明天银行开门吗」「下周一要上班吗」这类问题，模型自己按日历推常会算错补休、夹在两个节日之间的休息日。
// 节假日的规则用成熟的库（rickar/cal），这里只按国家代码选一套、把结果写成给模型看的话。
package holidays

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rickar/cal/v2"
	"github.com/rickar/cal/v2/jp"
)

// country 是一个国家的节假日：给模型看的名字和规则。
type country struct {
	name     string
	holidays []*cal.Holiday
}

// countries 是支持的国家（ISO 3166-1 两位代码）。要加别的国家时在这里加一行（库里每个国家一个包）。
var countries = map[string]country{
	"JP": {name: "日本", holidays: jp.Holidays},
}

// Calendar 是一个国家的节假日日历。
type Calendar struct {
	country string
	name    string
	cal     *cal.Calendar
	now     func() time.Time
}

// ForCountry 按国家代码（大小写都行）建日历；不支持的代码报错（写明支持哪些）。
func ForCountry(code string) (*Calendar, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	found, ok := countries[code]
	if !ok {
		supported := make([]string, 0, len(countries))
		for known := range countries {
			supported = append(supported, known)
		}
		slices.Sort(supported)
		return nil, fmt.Errorf("不支持的国家代码 %q（支持：%s）", code, strings.Join(supported, "、"))
	}
	calendar := &cal.Calendar{}
	calendar.AddHoliday(found.holidays...)
	return &Calendar{country: code, name: found.name, cal: calendar, now: time.Now}, nil
}

// Country 是这本日历的国家代码。
func (c *Calendar) Country() string { return c.country }

// Holiday 是 day 那天的节假日名字（库里的英文名；补休写成「某某节的补休」）；不是节假日时 ok = false。按 day 所在时区的日期算。
func (c *Calendar) Holiday(day time.Time) (name string, ok bool) {
	actual, observed, holiday := c.cal.IsHoliday(day)
	switch {
	case holiday == nil || !actual && !observed:
		return "", false
	case !actual: // 原本的那天落在周日等，挪到这一天休
		return holiday.Name + " 的补休", true
	}
	return holiday.Name, true
}

// Brief 是给模型的说明：今天、明天是节假日时各一行，都不是时没有（平常不多占地方）。按服务端的时区算日期，
// 和每轮告诉模型的「现在是……」一致。
func (c *Calendar) Brief(string) []string {
	now := c.now()
	var lines []string
	for _, day := range []struct {
		label string
		date  time.Time
	}{{"今天", now}, {"明天", now.AddDate(0, 0, 1)}} {
		if name, ok := c.Holiday(day.date); ok {
			lines = append(lines, fmt.Sprintf("%s是%s的节假日：%s。", day.label, c.name, name))
		}
	}
	return lines
}
