package tools

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// 派出后告诉主人最长等多久：按调查给的最长时间说分钟数，往上取整。
func TestLongestWaitNoteRoundsUp(t *testing.T) {
	for _, c := range []struct {
		longest time.Duration
		minutes int
	}{{3*time.Minute + 45*time.Second, 4}, {2*time.Minute + 15*time.Second, 3}, {5 * time.Minute, 5}} {
		if note := longestWaitNote(c.longest); !strings.Contains(note, "约 "+strconv.Itoa(c.minutes)+" 分钟") {
			t.Errorf("最长 %v：应说约 %d 分钟：%q", c.longest, c.minutes, note)
		}
	}
}
