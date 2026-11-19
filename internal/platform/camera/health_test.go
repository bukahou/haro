package camera

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bukahou/haro/internal/terminal"
)

// syncBuffer 是并发安全的日志缓冲。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

func testLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// 只在状态变化时写日志：连续失败只记一条，恢复记一条。
func TestHealthLogsOnlyTransitions(t *testing.T) {
	var logs syncBuffer
	h := newHealth("EMEET", testLogger(&logs))
	if h.currentName() != "EMEET" {
		t.Fatalf("一开始应能用：%q", h.currentName())
	}
	h.markUp("EMEET") // 正常出画面：不写日志
	for range 5 {
		h.markDown("摄像头进程退出", errors.New("No such device"))
	}
	if n := logs.count("摄像头不可用"); n != 1 || h.currentName() != "" {
		t.Fatalf("连续失败应只记一条（记了 %d 条），且不可用：%q", n, h.currentName())
	}
	h.markUp("EMEET")
	h.markUp("EMEET")
	if n := logs.count("摄像头恢复了"); n != 1 || h.currentName() != "EMEET" {
		t.Errorf("恢复应只记一条（记了 %d 条），且能用：%q", n, h.currentName())
	}
}

// ffmpeg 备用：没有摄像头时每次取帧都先重新找（插回来就能接上），不可用只记一条。
func TestFFmpegCameraRelocatesEveryTimeWhileMissing(t *testing.T) {
	var logs syncBuffer
	var calls atomic.Int32
	locate := func(context.Context) ([]string, string, bool) { calls.Add(1); return nil, "", false }
	c := NewFFmpegCamera(locate, "EMEET", 640, testLogger(&logs))
	for range 3 {
		if _, err := c.Capture(context.Background(), 0); !errors.Is(err, terminal.ErrNoCamera) {
			t.Fatalf("没有摄像头时应报 ErrNoCamera：%v", err)
		}
	}
	if calls.Load() != 3 || logs.count("摄像头不可用") != 1 || c.CurrentName() != "" {
		t.Errorf("每次都应重新找（%d 次）、只记一条（%d 条）、报告不可用（%q）",
			calls.Load(), logs.count("摄像头不可用"), c.CurrentName())
	}
}
