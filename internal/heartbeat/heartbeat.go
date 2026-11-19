// Package heartbeat 让 WebSocket 连接的两端各自定时 ping：等不到 pong 就当连接已经断了。
//
// 只靠 TCP 时，对端掉电、网线被拔、中间的设备丢了连接，这类「半开」的连接要很久才发现，
// 终端会一直以为自己连着；心跳把发现的时间压到 Interval + Timeout 以内。
package heartbeat

import (
	"context"
	"fmt"
	"time"
)

// Pinger 是能 ping 对端、收到 pong 才返回的连接。coder/websocket 的 *Conn 满足它
// （它只在有人读连接时处理 pong，所以 Keep 运行期间连接上必须有读循环）。
type Pinger interface {
	Ping(ctx context.Context) error
}

// Timing 是心跳的间隔和超时。
type Timing struct {
	Interval time.Duration // 多久 ping 一次
	Timeout  time.Duration // 一次 ping 最多等多久的 pong
}

// Default 是终端和服务端约定的心跳：每 10 秒 ping 一次，5 秒内等不到 pong 就断开。
var Default = Timing{Interval: 10 * time.Second, Timeout: 5 * time.Second}

// OrDefault 返回 t；t 是零值时返回 Default。
func (t Timing) OrDefault() Timing {
	if t == (Timing{}) {
		return Default
	}
	return t
}

// Keep 每隔 Interval ping 一次，直到 ctx 结束（返回 nil），或者某一次 Timeout 内等不到 pong（返回错误）。
func Keep(ctx context.Context, conn Pinger, timing Timing) error {
	ticker := time.NewTicker(timing.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, timing.Timeout)
		err := conn.Ping(pingCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("心跳：%s 内没等到对端回应: %w", timing.Timeout, err)
		}
	}
}
