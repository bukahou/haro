package brain

import (
	"testing"
	"time"
)

func TestClockBriefer(t *testing.T) {
	briefing := ClockBriefer{Now: func() time.Time { return time.Date(2026, 11, 17, 9, 5, 0, 0, time.Local) }}.Brief("主人")
	lines := briefing.Lines
	if briefing.Delivered != nil || len(lines) != 1 || lines[0] != "现在是 2026年11月17日（星期二）09:05。" {
		t.Errorf("%v", lines)
	}
}
