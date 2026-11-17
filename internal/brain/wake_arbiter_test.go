package brain

import (
	"sync"
	"testing"
	"time"
)

func TestWakeArbiterSingleTerminalDoesNotWait(t *testing.T) {
	arbiter := NewWakeArbiter(time.Second)
	only := &Session{}
	arbiter.join(only)
	started := time.Now()
	if !arbiter.claim(WakeClaim{Score: 0.3, terminal: only}) || time.Since(started) > 100*time.Millisecond {
		t.Errorf("只有一台在线时应立即回答（等了 %v）", time.Since(started))
	}
}

func TestWakeArbiterPicksBestClaim(t *testing.T) {
	cases := []struct {
		name        string
		first, next WakeClaim
		wantFirst   bool
	}{
		{"声纹分数高的赢", WakeClaim{Score: 0.6, Level: 0.01}, WakeClaim{Score: 0.3, Level: 0.05}, true},
		{"分数差不多时比音量", WakeClaim{Score: 0.50, Level: 0.01}, WakeClaim{Score: 0.53, Level: 0.04}, false},
		{"正在播放的那台被叫（打断）最优先", WakeClaim{Score: 0.2, Barge: true}, WakeClaim{Score: 0.7}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			arbiter := NewWakeArbiter(50 * time.Millisecond)
			a, b := &Session{}, &Session{}
			arbiter.join(a)
			arbiter.join(b)
			c.first.terminal, c.next.terminal = a, b
			var wonA, wonB bool
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); wonA = arbiter.claim(c.first) }()
			time.Sleep(10 * time.Millisecond) // 第二台稍晚一点听到（在窗口之内）
			go func() { defer wg.Done(); wonB = arbiter.claim(c.next) }()
			wg.Wait()
			if wonA != c.wantFirst || wonB == wonA {
				t.Errorf("第一台赢=%v 第二台赢=%v，应为第一台赢=%v 且只有一台赢", wonA, wonB, c.wantFirst)
			}
		})
	}
}

func TestWakeArbiterLeavingTerminalStopsWaiting(t *testing.T) {
	arbiter := NewWakeArbiter(time.Second)
	a, b := &Session{}, &Session{}
	arbiter.join(a)
	arbiter.join(b)
	arbiter.leave(b) // 另一台断开了：又只剩一台
	started := time.Now()
	if !arbiter.claim(WakeClaim{terminal: a}) || time.Since(started) > 100*time.Millisecond {
		t.Error("另一台离开后应不再等")
	}
}
