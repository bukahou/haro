package assistant

import (
	"context"
	"errors"
	"io"
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

// runHandsFree 是 v2.0 的免按键模式：按回车进入对话模式，之后自动轮流；再按回车退出。
func (a *Assistant) runHandsFree(ctx context.Context) error {
	for {
		if err := a.parts.Toggle.WaitForEnter(ctx); err != nil {
			return err
		}
		err := a.converse(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, io.EOF) {
			return err
		}
		if err != nil {
			a.logger.Error("对话模式异常退出", "err", err)
		}
	}
}

// converse 运行一次对话模式，直到用户退出（返回 nil）或出错。
// 半双工（V3 ①）：从请求回答到播完再过 EchoGuard，上传的是静音，haro 听不到自己的声音，也听不到用户插话。
func (a *Assistant) converse(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	leave := make(chan error, 1)
	go func() {
		leave <- a.parts.Toggle.WaitForLeave(ctx)
		cancel()
	}()

	mic, err := a.parts.Microphone.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer mic.Close()
	uploadDone := make(chan error, 1)
	go func() { uploadDone <- a.uploadMicrophone(ctx, mic) }()

	for {
		var event realtimeproto.ServerEvent
		select {
		case <-ctx.Done():
			select {
			case err := <-leave:
				return err
			default:
				return ctx.Err()
			}
		case err := <-uploadDone:
			return err
		case event = <-a.parts.Brain.Events():
		}
		switch e := event.(type) {
		case realtimeproto.InputAudioBufferSpeechStarted:
			a.logger.Debug("听到说话", "audio_start_ms", e.AudioStartMs)
		case realtimeproto.InputAudioBufferSpeechStopped:
			a.respondHandsFree(ctx, a.uplink.wallClockAt(e.AudioEndMs))
		case realtimeproto.HaroNotice:
			a.playNotice(ctx, e)
		case realtimeproto.Error:
			a.logger.Error("服务端出错", "message", e.Message)
		default:
			// 上一轮被取消后迟到的回答事件等，忽略
		}
	}
}

// respondHandsFree 处理一轮：带上「说完那一刻」的画面请求回答，播完后恢复收音。返回是否被打断。
//
// 播放期间不能听用户说话，否则会把自己的声音当成用户说的（没有回声消除）：
//   - 可以打断时（v3 I-1）：麦克风照常上传，告诉语音前端「正在播放」，它只听唤醒词；
//   - 不能打断时（半双工，V3 ①）：上传静音。
func (a *Assistant) respondHandsFree(ctx context.Context, speechEnd time.Time) (interrupted bool) {
	a.busy.Store(true)
	defer a.busy.Store(false)
	bargeIn := a.opts.BargeIn && a.opts.WakeWord != nil
	if bargeIn {
		if err := a.parts.Brain.Send(ctx, realtimeproto.HaroPlaybackStarted{}); err != nil {
			a.logger.Error("通知开始播放失败", "err", err)
		}
	} else {
		a.uplink.setMuted(true)
	}
	defer func() {
		if !interrupted {
			select { // 播完再等一会儿，让音箱里的余音散掉
			case <-time.After(a.opts.EchoGuard):
			case <-ctx.Done():
			}
		}
		if bargeIn {
			if err := a.parts.Brain.Send(context.WithoutCancel(ctx), realtimeproto.HaroPlaybackStopped{}); err != nil {
				a.logger.Error("通知播放结束失败", "err", err)
			}
		} else {
			a.uplink.setMuted(false)
		}
	}()

	ctx, span := tracer.Start(ctx, "haro.turn")
	defer span.End()
	delay := time.Since(speechEnd)
	span.SetAttributes(attribute.Float64("haro.turn.endpoint_delay", delay.Seconds()))
	endpointDelay.Record(ctx, delay.Seconds())
	a.logger.Info("⏱ 判定说完", "endpoint_delay", delay.Round(time.Millisecond))

	brain := a.parts.Brain
	if err := a.attachFrame(ctx, span); err != nil {
		a.logger.Error("提交画面失败", "err", err)
		return false
	}
	if err := brain.Send(ctx, realtimeproto.ResponseCreate{}); err != nil {
		a.logger.Error("请求回答失败", "err", err)
		return false
	}
	err := a.playResponse(ctx, span, speechEnd)
	if errors.Is(err, errInterrupted) {
		return true
	}
	if err != nil && ctx.Err() == nil {
		a.logger.Error("本轮对话失败", "err", err)
	}
	return false
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

// ErrMicrophoneStopped：麦克风流自己停了（被拔掉、蓝牙断开）。终端的看守据此回到「等设备」（v4 D3）。
var ErrMicrophoneStopped = errors.New("麦克风意外停止")

// micUplink 记录已上传音频的时间轴，用于把 VAD 给出的「音频时间」换算成墙上时间。
// 时间轴跨对话模式累计（服务端的转写会话在整个程序生命周期内只有一个）。
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
