package assistant

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/telemetry"
)

// endpointDelay：用户真正说完 → VAD 判定说完（主要是等静音的时间）。
var endpointDelay = telemetry.MustLatencyHistogram(otel.Meter("github.com/bukahou/haro/assistant"),
	"haro.turn.endpoint_delay", "用户说完到判定说完的时间（等静音 + 判断）")

// respondHandsFree 处理一轮（item 是服务端给这句话的编号，日志里和服务端的「一轮结束」对得上）：
// 服务端判定说完时已经自己开始回答（画面由它要的时候经 MCP 来拍），这里负责播放，播完后恢复收音。返回是否被打断。
func (a *Assistant) respondHandsFree(ctx context.Context, item string, speechEnd time.Time) (interrupted bool) {
	a.busy.Store(true)
	defer a.busy.Store(false)
	resume := a.suppressEcho(ctx)
	defer func() { resume(!interrupted) }() // 被打断时主人已经在说了：不等余音

	ctx, span := tracer.Start(ctx, "haro.turn")
	defer span.End()
	delay := time.Since(speechEnd)
	span.SetAttributes(attribute.Float64("haro.turn.endpoint_delay", delay.Seconds()))
	endpointDelay.Record(ctx, delay.Seconds())
	a.logger.Info("⏱ 判定说完", "item", item, "endpoint_delay", delay.Round(time.Millisecond))

	err := a.playResponse(ctx, span, item, speechEnd)
	if errors.Is(err, errInterrupted) {
		return true
	}
	if err != nil && ctx.Err() == nil {
		a.logger.Error("本轮对话失败", "err", err)
	}
	return false
}

// suppressEcho 在终端出声（回答、醒着时的提示音）之前调用：出声期间不能听主人说话，
// 否则会把自己的声音当成主人说的（没有回声消除）。
//   - 可以打断时：麦克风照常上传，告诉语音前端「正在播放」，它只听唤醒词；
//   - 不能打断时（半双工）：上传静音。
//
// 返回的 resume 在出声结束后调用：settle 为 true 时先等 EchoGuard（音箱里的余音散掉）再恢复收听。
func (a *Assistant) suppressEcho(ctx context.Context) (resume func(settle bool)) {
	if a.opts.BargeIn {
		if err := a.parts.Brain.Send(ctx, realtimeproto.HaroPlaybackStarted{}); err != nil {
			a.logger.Error("通知开始播放失败", "err", err)
		}
	} else {
		a.uplink.setMuted(true)
	}
	return func(settle bool) {
		if settle {
			select {
			case <-time.After(a.opts.EchoGuard):
			case <-ctx.Done():
			}
		}
		if a.opts.BargeIn {
			if err := a.parts.Brain.Send(context.WithoutCancel(ctx), realtimeproto.HaroPlaybackStopped{}); err != nil {
				a.logger.Error("通知播放结束失败", "err", err)
			}
		} else {
			a.uplink.setMuted(false)
		}
	}
}

// uploadMicrophone 把麦克风的每一块音频交给服务端；静音期间上传等长的静音，保持时间轴连续。
func (a *Assistant) uploadMicrophone(ctx context.Context, mic MicrophoneStream) error {
	for chunk := range mic.Chunks() {
		if a.uplink.muted() {
			chunk.Data = make([]byte, len(chunk.Data))
		}
		if err := a.parts.Brain.Send(ctx, realtimeproto.InputAudioBufferAppend{Audio: chunk}); err != nil {
			return err
		}
		a.uplink.advance(chunk)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrMicrophoneStopped
}

// ErrMicrophoneStopped：麦克风流自己停了（被拔掉、蓝牙断开）。终端的看守据此回到「等设备」。
var ErrMicrophoneStopped = errors.New("麦克风意外停止")

// micUplink 记录已上传音频的时间轴，用于把 VAD 给出的「音频时间」换算成墙上时间。
// 时间轴在整条连接上累计（服务端的转写会话在连接期间只有一个）。
type micUplink struct {
	mu       sync.Mutex
	sentMs   int64
	lastSent time.Time
	isMuted  bool
}

func (u *micUplink) advance(chunk realtimeproto.PCMAudio) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.sentMs += chunk.Duration().Milliseconds()
	u.lastSent = time.Now()
}

// wallClockAt 估算音频时间轴上 audioMs 处对应的墙上时间（麦克风按实时速度产出，线性换算即可）。
func (u *micUplink) wallClockAt(audioMs int64) time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastSent.Add(-time.Duration(u.sentMs-audioMs) * time.Millisecond)
}

func (u *micUplink) setMuted(muted bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.isMuted = muted
}

func (u *micUplink) muted() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.isMuted
}
