package heartbeat

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// answering 每次 ping 都马上回应。
type answering struct{ pings atomic.Int32 }

func (a *answering) Ping(context.Context) error {
	a.pings.Add(1)
	return nil
}

// silent 从不回应：ping 一直等到超时。
type silent struct{}

func (silent) Ping(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

var fast = Timing{Interval: 10 * time.Millisecond, Timeout: 20 * time.Millisecond}

func TestKeepPingsUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	peer := &answering{}
	if err := Keep(ctx, peer, fast); err != nil {
		t.Fatalf("对端一直回应时，取消后应返回 nil：%v", err)
	}
	if peer.pings.Load() < 3 {
		t.Errorf("100ms 里按 10ms 间隔应 ping 好几次，只 ping 了 %d 次", peer.pings.Load())
	}
}

func TestKeepFailsWhenPeerStopsAnswering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if err := Keep(ctx, silent{}, fast); err == nil {
		t.Fatal("对端不回应时应返回错误")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Errorf("应在间隔 + 超时（约 30ms）后发现，用了 %s", elapsed)
	}
}

func TestZeroTimingMeansDefault(t *testing.T) {
	if (Timing{}).OrDefault() != Default || fast.OrDefault() != fast {
		t.Error("零值应换成默认值，非零值保持不变")
	}
}
