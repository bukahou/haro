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
	"time"
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

// 常驻模式：找不到摄像头时一直重试找（拔掉期间），但只记一条日志。
func TestContinuousCameraKeepsLookingQuietly(t *testing.T) {
	var logs syncBuffer
	var calls atomic.Int32
	locate := func(context.Context) ([]string, string, bool) { calls.Add(1); return nil, "", false }
	c := NewContinuousCamera(locate, "EMEET", 640, 2, 3*time.Second, testLogger(&logs))
	c.retryMin, c.retryMax = time.Millisecond, 2*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if calls.Load() < 5 {
		t.Fatalf("应反复重新找摄像头：%d 次", calls.Load())
	}
	if n := logs.count("摄像头不可用"); n != 1 || c.CurrentName() != "" {
		t.Errorf("应只记一条（记了 %d 条），且报告不可用：%q", n, c.CurrentName())
	}
}

// 按需模式：没有摄像头时每次取帧都先重新找（插回来就能接上），不可用只记一条。
func TestOnDemandCameraRelocatesEveryTimeWhileMissing(t *testing.T) {
	var logs syncBuffer
	var calls atomic.Int32
	locate := func(context.Context) ([]string, string, bool) { calls.Add(1); return nil, "", false }
	c := NewOnDemandCamera(locate, "EMEET", 640, testLogger(&logs))
	for range 3 {
		if _, _, err := c.LatestFrame(context.Background()); !errors.Is(err, errNoCamera) {
			t.Fatalf("没有摄像头时应报 errNoCamera：%v", err)
		}
	}
	if calls.Load() != 3 || logs.count("摄像头不可用") != 1 || c.CurrentName() != "" {
		t.Errorf("每次都应重新找（%d 次）、只记一条（%d 条）、报告不可用（%q）",
			calls.Load(), logs.count("摄像头不可用"), c.CurrentName())
	}
}
