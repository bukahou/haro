package buildinfo

import (
	"strings"
	"testing"
)

func TestMismatch(t *testing.T) {
	cases := []struct {
		ours, theirs string
		want         bool
	}{
		{"v0.5.0-1a2b3c4", "v0.5.0-1a2b3c4", false},
		{"v0.5.0-1a2b3c4", "v0.5.0-9f8e7d6", true},
		{"v0.5.0-1a2b3c4", "", true}, // 对方是不报版本的旧程序
		{DevVersion, "v0.5.0-1a2b3c4", false},
		{"v0.5.0-1a2b3c4", DevVersion, false},
		{DevVersion, "", false},
	}
	for _, c := range cases {
		if got := Mismatch(c.ours, c.theirs); got != c.want {
			t.Errorf("Mismatch(%q, %q) = %v，应为 %v", c.ours, c.theirs, got, c.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	line := Describe("haro")
	if !strings.HasPrefix(line, "haro "+Version+" (") {
		t.Fatalf("Describe = %q", line)
	}
}
