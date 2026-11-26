package reminders

import (
	"strings"
	"testing"
	"time"
)

var tokyo = time.FixedZone("JST", 9*3600)

func TestParseRecurrenceDaily(t *testing.T) {
	first := time.Date(2026, 11, 27, 8, 0, 0, 0, tokyo)
	rule, err := parseRecurrence("FREQ=DAILY", first, tokyo)
	if err != nil {
		t.Fatal(err)
	}
	next, ok := rule.after(first)
	if want := time.Date(2026, 11, 28, 8, 0, 0, 0, tokyo); !ok || !next.Equal(want) {
		t.Errorf("每天 8 点：第一次之后是 %v，应为 %v", next, want)
	}
	// 按服务端时区算钟点：第一次之后过了几天再问，下一次照样是 8 点
	if next, _ := rule.after(time.Date(2026, 12, 3, 9, 0, 0, 0, tokyo)); !next.Equal(time.Date(2026, 12, 4, 8, 0, 0, 0, tokyo)) {
		t.Errorf("过了几天再问：%v", next.In(tokyo))
	}
}

func TestParseRecurrenceWeeklyWithPrefixAndCount(t *testing.T) {
	first := time.Date(2026, 11, 30, 7, 30, 0, 0, tokyo) // 周一
	rule, err := parseRecurrence("RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=2", first, tokyo)
	if err != nil {
		t.Fatal(err)
	}
	second, ok := rule.after(first)
	if !ok || !second.Equal(time.Date(2026, 12, 7, 7, 30, 0, 0, tokyo)) {
		t.Errorf("第二次应在下周一：%v", second)
	}
	if _, ok := rule.after(second); ok {
		t.Error("COUNT=2：第二次之后不应再到点")
	}
}

func TestParseRecurrenceRejects(t *testing.T) {
	monday := time.Date(2026, 11, 30, 7, 30, 0, 0, tokyo)
	for _, c := range []struct {
		name, rule string
		first      time.Time
		want       string
	}{
		{"不是 RRULE", "每天", monday, "不是合法"},
		{"自带 DTSTART", "DTSTART:20261130T073000Z\nFREQ=DAILY", monday, "DTSTART"},
		{"太密", "FREQ=MINUTELY", monday, "太近"},
		{"第一次不符合规则", "FREQ=WEEKLY;BYDAY=TU", monday, "不符合"},
	} {
		if _, err := parseRecurrence(c.rule, c.first, tokyo); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应拒绝（含「%s」），得到 %v", c.name, c.want, err)
		}
	}
	// 阳性对照：每半小时可以
	if _, err := parseRecurrence("FREQ=MINUTELY;INTERVAL=30", monday, tokyo); err != nil {
		t.Errorf("每半小时应能定：%v", err)
	}
}
