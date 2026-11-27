package holidays

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func day(t *testing.T, text string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation(time.DateOnly, text, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// 日本的节假日：普通的、补休（宪法纪念日落在周日，挪到 5 月 6 日）、夹在两个节日之间的国民の休日；平日不是。
func TestJapanHolidays(t *testing.T) {
	calendar, err := ForCountry("jp")
	if err != nil {
		t.Fatal(err)
	}
	for date, want := range map[string]string{
		"2026-11-23": "Labor Thanksgiving Day",
		"2026-05-06": "Constitution Memorial Day 的补休",
		"2026-09-22": "National holiday between Respect for the Aged Day and Autumnal Equinox Day",
		"2026-11-24": "",
	} {
		name, ok := calendar.Holiday(day(t, date))
		if name != want || ok != (want != "") {
			t.Errorf("%s：%q %v，应是 %q", date, name, ok, want)
		}
	}
}

// 说明只在今天或明天是节假日时才有，两天都是时两行；平常没有。
func TestBriefOnlyOnHolidays(t *testing.T) {
	calendar, err := ForCountry("JP")
	if err != nil {
		t.Fatal(err)
	}
	for now, want := range map[string][]string{
		"2026-11-22": {"明天是节假日（JP）：Labor Thanksgiving Day。"},
		"2026-09-21": {"今天是节假日（JP）：Respect for the Aged Day。",
			"明天是节假日（JP）：National holiday between Respect for the Aged Day and Autumnal Equinox Day。"},
		"2026-11-25": nil,
	} {
		at := day(t, now).Add(10 * time.Hour)
		calendar.now = func() time.Time { return at }
		if got := calendar.Brief(""); !slices.Equal(got, want) {
			t.Errorf("%s：%q，应是 %q", now, got, want)
		}
	}
}

// 别的国家只改配置：库里有全国节假日的都登记了（例如美国的独立日、德国的统一日）。
func TestOtherCountries(t *testing.T) {
	for code, date := range map[string]string{"US": "2026-07-03", "de": "2026-10-03"} {
		calendar, err := ForCountry(code)
		if err != nil {
			t.Fatal(err)
		}
		if name, ok := calendar.Holiday(day(t, date)); !ok {
			t.Errorf("%s 的 %s 应是节假日：%q", code, date, name)
		}
	}
}

// 不支持的国家代码报错，写明支持哪些。
func TestUnknownCountry(t *testing.T) {
	if _, err := ForCountry("XX"); err == nil || !strings.Contains(err.Error(), "JP") {
		t.Errorf("不支持的代码应报错并列出支持的：%v", err)
	}
}
