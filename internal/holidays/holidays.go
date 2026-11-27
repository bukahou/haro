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
	"github.com/rickar/cal/v2/ar"
	"github.com/rickar/cal/v2/at"
	"github.com/rickar/cal/v2/be"
	"github.com/rickar/cal/v2/bg"
	"github.com/rickar/cal/v2/bj"
	"github.com/rickar/cal/v2/br"
	"github.com/rickar/cal/v2/ca"
	"github.com/rickar/cal/v2/ch"
	"github.com/rickar/cal/v2/cy"
	"github.com/rickar/cal/v2/cz"
	"github.com/rickar/cal/v2/de"
	"github.com/rickar/cal/v2/dk"
	"github.com/rickar/cal/v2/ee"
	"github.com/rickar/cal/v2/es"
	"github.com/rickar/cal/v2/fi"
	"github.com/rickar/cal/v2/fr"
	"github.com/rickar/cal/v2/gb"
	"github.com/rickar/cal/v2/gr"
	"github.com/rickar/cal/v2/hr"
	"github.com/rickar/cal/v2/hu"
	"github.com/rickar/cal/v2/ie"
	"github.com/rickar/cal/v2/is"
	"github.com/rickar/cal/v2/it"
	"github.com/rickar/cal/v2/jp"
	"github.com/rickar/cal/v2/ke"
	"github.com/rickar/cal/v2/lt"
	"github.com/rickar/cal/v2/lu"
	"github.com/rickar/cal/v2/lv"
	"github.com/rickar/cal/v2/mt"
	"github.com/rickar/cal/v2/mw"
	"github.com/rickar/cal/v2/mx"
	"github.com/rickar/cal/v2/nc"
	"github.com/rickar/cal/v2/nl"
	"github.com/rickar/cal/v2/no"
	"github.com/rickar/cal/v2/nz"
	"github.com/rickar/cal/v2/pl"
	"github.com/rickar/cal/v2/pt"
	"github.com/rickar/cal/v2/ro"
	"github.com/rickar/cal/v2/rs"
	"github.com/rickar/cal/v2/ru"
	"github.com/rickar/cal/v2/se"
	"github.com/rickar/cal/v2/si"
	"github.com/rickar/cal/v2/sk"
	"github.com/rickar/cal/v2/th"
	"github.com/rickar/cal/v2/ua"
	"github.com/rickar/cal/v2/us"
	"github.com/rickar/cal/v2/za"
)

// countries 是支持的国家（ISO 3166-1 两位代码）→ 全国的节假日：库里有全国节假日的都登记了（库里一个国家一个包）。
// AU 只有各州的节假日（没有全国的一套），要用时按州加（例如 ISO 3166-2 的 AU-NSW）。
var countries = map[string][]*cal.Holiday{
	"AR": ar.Holidays,
	"AT": at.Holidays,
	"BE": be.Holidays,
	"BG": bg.Holidays,
	"BJ": bj.Holidays,
	"BR": br.Holidays,
	"CA": ca.Holidays,
	"CH": ch.Holidays,
	"CY": cy.Holidays,
	"CZ": cz.Holidays,
	"DE": de.Holidays,
	"DK": dk.Holidays,
	"EE": ee.Holidays,
	"ES": es.Holidays,
	"FI": fi.Holidays,
	"FR": fr.Holidays,
	"GB": gb.Holidays,
	"GR": gr.Holidays,
	"HR": hr.Holidays,
	"HU": hu.Holidays,
	"IE": ie.Holidays,
	"IS": is.Holidays,
	"IT": it.Holidays,
	"JP": jp.Holidays,
	"KE": ke.Holidays,
	"LT": lt.Holidays,
	"LU": lu.Holidays,
	"LV": lv.Holidays,
	"MT": mt.Holidays,
	"MW": mw.Holidays,
	"MX": mx.Holidays,
	"NC": nc.Holidays,
	"NL": nl.Holidays,
	"NO": no.Holidays,
	"NZ": nz.Holidays,
	"PL": pl.Holidays,
	"PT": pt.Holidays,
	"RO": ro.Holidays,
	"RS": rs.Holidays,
	"RU": ru.Holidays,
	"SE": se.Holidays,
	"SI": si.Holidays,
	"SK": sk.Holidays,
	"TH": th.Holidays,
	"UA": ua.Holidays,
	"US": us.Holidays,
	"ZA": za.Holidays,
}

// Calendar 是一个国家的节假日日历。
type Calendar struct {
	country string
	cal     *cal.Calendar
	now     func() time.Time
}

// ForCountry 按国家代码（大小写都行）建日历；不支持的代码报错（写明支持哪些）。
func ForCountry(code string) (*Calendar, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	holidays, ok := countries[code]
	if !ok {
		supported := make([]string, 0, len(countries))
		for known := range countries {
			supported = append(supported, known)
		}
		slices.Sort(supported)
		return nil, fmt.Errorf("不支持的国家代码 %q（支持：%s）", code, strings.Join(supported, "、"))
	}
	calendar := &cal.Calendar{}
	calendar.AddHoliday(holidays...)
	return &Calendar{country: code, cal: calendar, now: time.Now}, nil
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
			lines = append(lines, fmt.Sprintf("%s是节假日（%s）：%s。", day.label, c.country, name))
		}
	}
	return lines
}
