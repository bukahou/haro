package tones

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEmbeddedTonesParse(t *testing.T) {
	for name, load := range map[string]func() (any, time.Duration, error){
		"offline": func() (any, time.Duration, error) { a, err := Offline(); return a.Format, a.Duration(), err },
		"online":  func() (any, time.Duration, error) { a, err := Online(); return a.Format, a.Duration(), err },
	} {
		format, duration, err := load()
		if err != nil || duration < 500*time.Millisecond || duration > 5*time.Second {
			t.Errorf("%s：%v %v %v", name, format, duration, err)
		}
	}
}

func TestLoadTonesFromDirectory(t *testing.T) {
	builtin, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if d := builtin.Sleep.Duration(); d < 500*time.Millisecond || d > 3*time.Second {
		t.Errorf("内置的休眠提示音长度不对：%v", d)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sleep.wav"), offlineWAV, 0o600); err != nil { // 用断开的声音冒充「去睡觉了」
		t.Fatal(err)
	}
	tones, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tones.Sleep.Data) != len(builtin.Offline.Data) {
		t.Error("目录里有 sleep.wav 时应该用它")
	}
	if len(tones.Online.Data) != len(builtin.Online.Data) || len(tones.Offline.Data) != len(builtin.Offline.Data) {
		t.Error("目录里没有的应该用内置的")
	}
	if err := os.WriteFile(filepath.Join(dir, "offline.wav"), []byte("坏文件"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("坏的 WAV 应报错")
	}
}
