package buildinfo

import (
	"bytes"
	"encoding/json"
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

// -version -json 给机器读：一行 JSON，字段是程序名、版本号、系统、架构、Go 版本；不加 -json 时是给人看的那一行。
func TestWriteVersion(t *testing.T) {
	var out bytes.Buffer
	if err := WriteVersion(&out, "haro", true); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Errorf("应只有一行：%q", out.String())
	}
	var info Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil || info != Report("haro") || info.Version != Version || info.Program != "haro" {
		t.Errorf("JSON 解出来应是这个程序的版本信息：%+v %v（%s）", info, err, out.String())
	}
	out.Reset()
	if err := WriteVersion(&out, "haro", false); err != nil || out.String() != Describe("haro")+"\n" {
		t.Errorf("不加 -json 时是给人看的那一行：%q %v", out.String(), err)
	}
}
