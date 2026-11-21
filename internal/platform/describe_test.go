package platform

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/platform/audio"
	"github.com/bukahou/haro/internal/platform/osdiff"
	"github.com/bukahou/haro/internal/terminal"
)

// fakeOS 按设备 ID 报属性；err 不为空时什么都认不出。
type fakeOS struct {
	traits map[string]osdiff.AudioTraits
	err    error
}

func (f fakeOS) AudioTraits(_ context.Context, id string) (osdiff.AudioTraits, error) {
	return f.traits[id], f.err
}
func (fakeOS) Cameras(context.Context) ([]terminal.Device, error) { return nil, nil }
func (fakeOS) CameraInput(context.Context, string, string) osdiff.CameraCapture {
	return osdiff.CameraCapture{}
}

// warnCounter 数警告级别的日志记录（带 err 属性的）。
type warnCounter struct {
	mu    sync.Mutex
	warns int
}

func (c *warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (c *warnCounter) Handle(_ context.Context, record slog.Record) error {
	hasErr := false
	record.Attrs(func(attr slog.Attr) bool {
		hasErr = hasErr || attr.Key == "err"
		return !hasErr
	})
	if record.Level == slog.LevelWarn && hasErr {
		c.mu.Lock()
		c.warns++
		c.mu.Unlock()
	}
	return nil
}
func (c *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *warnCounter) WithGroup(string) slog.Handler      { return c }

func (c *warnCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.warns
}

// 回录源按系统报的属性去掉，接口类型照报的填；认不出时设备照常列出、类型未知，提醒只写一次。
func TestDescribeUsesSystemTraits(t *testing.T) {
	listed := []audio.Device{{Key: "mic", Name: "USB 麦克风", Default: true}, {Key: "mic.monitor-like", Name: "回录"},
		{Key: "headset", Name: "蓝牙耳机"}}
	d := &LocalDevices{os: fakeOS{traits: map[string]osdiff.AudioTraits{
		"mic":              {Bus: terminal.BusUSB},
		"mic.monitor-like": {Bus: terminal.BusBuiltin, Monitor: true},
		"headset":          {Bus: terminal.BusBluetooth},
	}}, logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	got := d.describe(listed)
	if len(got) != 2 || got[0].ID != "mic" || got[0].Bus != terminal.BusUSB || !got[0].Default ||
		got[1].ID != "headset" || got[1].Bus != terminal.BusBluetooth {
		t.Errorf("回录源应去掉、接口类型照报的填：%+v", got)
	}

	warnings := &warnCounter{}
	blind := &LocalDevices{os: fakeOS{err: errors.New("pactl: not found")}, logger: slog.New(warnings)}
	for range 3 {
		if got := blind.describe(listed); len(got) != 3 || got[0].Bus != terminal.BusUnknown {
			t.Fatalf("认不出时照常列出、类型未知：%+v", got)
		}
	}
	if n := warnings.count(); n != 1 {
		t.Errorf("提醒（带错误的警告）只写一次：写了 %d 次", n)
	}
}
