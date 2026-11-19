// Package terminal 是终端的看守：按状态机把终端一直跑下去，除非收到关机信号，否则不退出。
//
//	连服务端（断了就按 1→2→4…→30 秒的间隔重连）
//	  → 等麦克风（没有就待机，每 2 秒查一次）
//	  → 运行对话（assistant）；同时每 2 秒按选择规则重新选一次设备
//
// 运行中任何一处出错（服务端断了、麦克风没了、该换麦克风了），都关掉这条连接、退回「连服务端」重来。
// 服务端断开超过 10 秒，用本地提示音说一声；恢复时再说一声（断开时终端听不出唤醒词，只能主动说）。
// 协议版本和服务端不兼容时重连没用：提示一次，之后隔很久才再试，等人把两端升到同一版本。
package terminal

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Connection 是一条到服务端的连接。
type Connection interface {
	assistant.BrainSession
	Done() <-chan struct{} // 连接断了时关闭
	Close() error
}

// Devices 是某一刻的设备情况。
type Devices struct {
	Microphone    Choice
	HasMicrophone bool
	Speaker       Choice
	HasSpeaker    bool
	Camera        string // 现在能用的摄像头名字；空 = 没有
}

// Session 是一次对话运行（assistant.Assistant 满足它）。
type Session interface {
	Run(ctx context.Context) error
	Busy() bool
}

// Parts 是看守用到的各段能力，由 cmd/haro 组装。
type Parts struct {
	Dial               func(ctx context.Context) (Connection, error)
	Catalog            DeviceCatalog                                  // 列本机设备（平台层实现）；看守每 Poll 按规则选一次
	MicrophoneOverride string                                         // 配置指定的麦克风 ID（HARO_AUDIO_SOURCE）；空 = 按规则选
	SpeakerOverride    string                                         // 配置指定的音箱 ID（HARO_AUDIO_SINK）；空 = 跟系统默认
	CameraName         func() string                                  // 现在能用的摄像头名字；空 = 没有（nil = 这台终端不用摄像头）
	NewSession         func(conn Connection, devices Devices) Session // 用这条连接和这组设备创建一次对话运行
	ToneSink           assistant.AudioSink                            // 播提示音用（跟系统默认）
	Offline            realtimeproto.PCMAudio                         // 「我和服务端断开了……」
	Online             realtimeproto.PCMAudio                         // 「我回来了」
}

// Timing 是看守的各个时间。
type Timing struct {
	OfflineNotice time.Duration // 服务端断开这么久才提示（躲开 make deploy 这种几秒的重启）
	Poll          time.Duration // 多久查一次设备
	BackoffMin    time.Duration // 重连间隔：从这么久开始翻倍
	BackoffMax    time.Duration // 重连间隔上限
	// IncompatibleRetry 是协议版本不兼容时多久再试一次（要等人升级，频繁重试没用）；零值 = DefaultTiming 的
	IncompatibleRetry time.Duration
}

// DefaultTiming 是看守的默认时间。
var DefaultTiming = Timing{OfflineNotice: 10 * time.Second, Poll: 2 * time.Second,
	BackoffMin: time.Second, BackoffMax: 30 * time.Second, IncompatibleRetry: 5 * time.Minute}

// Supervisor 是终端的看守。
type Supervisor struct {
	parts  Parts
	timing Timing
	logger *slog.Logger
}

// NewSupervisor 创建看守。
func NewSupervisor(parts Parts, timing Timing, logger *slog.Logger) *Supervisor {
	if timing.IncompatibleRetry == 0 {
		timing.IncompatibleRetry = DefaultTiming.IncompatibleRetry
	}
	return &Supervisor{parts: parts, timing: timing, logger: logger}
}

// 一次运行为什么结束。
var (
	errBrainLost      = errors.New("和服务端的连接断了")
	errSwitchDevice   = errors.New("该换麦克风了")
	errShutdown       = errors.New("关机")
	microphoneStopped = assistant.ErrMicrophoneStopped
)

// offlineState 记服务端从什么时候连不上、有没有提示过。
type offlineState struct {
	since     time.Time
	announced bool
}

// Run 一直运行，直到 ctx 取消（关机信号）；正常关机返回 nil。
func (s *Supervisor) Run(ctx context.Context) error {
	offline := offlineState{since: time.Now()}
	for {
		conn, err := s.connect(ctx, &offline)
		if err != nil {
			return nil // 只有关机才会连不上而返回
		}
		if offline.announced {
			s.playTone(ctx, s.parts.Online, "服务端恢复了")
		}
		offline = offlineState{}
		reason := s.serve(ctx, conn)
		_ = conn.Close()
		switch {
		case ctx.Err() != nil || errors.Is(reason, errShutdown):
			return nil
		case errors.Is(reason, errBrainLost):
			s.logger.Warn("和服务端的连接断了：重连")
			offline = offlineState{since: time.Now()}
		}
	}
}

// connect 连服务端，连不上就按翻倍的间隔重试；断开超过 OfflineNotice 时提示一次。
func (s *Supervisor) connect(ctx context.Context, offline *offlineState) (Connection, error) {
	backoff := s.timing.BackoffMin
	for {
		conn, err := s.parts.Dial(ctx)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if offline.since.IsZero() { // 不是因为断线才来重连的（例如刚换完麦克风）：从现在开始算
			offline.since = time.Now()
		}
		if errors.Is(err, realtimeproto.ErrIncompatibleProtocol) {
			s.logger.Error("和服务端的协议版本不兼容：把终端和服务端升到同一版本", "err", err,
				"retry_in", s.timing.IncompatibleRetry)
			if !offline.announced {
				offline.announced = true
				s.playTone(ctx, s.parts.Offline, "协议版本不兼容")
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(s.timing.IncompatibleRetry):
			}
			continue
		}
		s.logger.Warn("连不上服务端", "err", err, "retry_in", backoff)
		retryAt := time.Now().Add(backoff)
		for time.Now().Before(retryAt) {
			wait := time.Until(retryAt)
			if !offline.announced {
				if untilNotice := time.Until(offline.since.Add(s.timing.OfflineNotice)); untilNotice <= 0 {
					// 到提示时间先立刻再连一次：服务端可能正好在两次重试之间起来了（make deploy 时服务端启动要好几秒，
					// 而重试间隔在翻倍，下一次可能要等到 15 秒以后）；还连不上才提示（躲开这类重启）
					if conn, err := s.parts.Dial(ctx); err == nil {
						return conn, nil
					}
					offline.announced = true
					s.playTone(ctx, s.parts.Offline, "服务端断开超过提示时间")
					continue
				} else {
					wait = min(wait, untilNotice)
				}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		backoff = min(backoff*2, s.timing.BackoffMax)
	}
}

// serve 用一条连接运行，直到它该结束；返回结束的原因。
func (s *Supervisor) serve(ctx context.Context, conn Connection) error {
	devices, err := s.waitForMicrophone(ctx, conn)
	if err != nil {
		return err
	}
	if err := conn.Send(ctx, report(devices)); err != nil {
		return errBrainLost
	}
	session := s.parts.NewSession(conn, devices)
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go s.watch(runCtx, cancel, conn, session, devices)
	err = session.Run(runCtx)
	cause := context.Cause(runCtx)
	switch {
	case ctx.Err() != nil:
		return errShutdown
	case errors.Is(cause, errBrainLost), isDone(conn):
		return errBrainLost
	case errors.Is(cause, errSwitchDevice):
		return errSwitchDevice
	case errors.Is(err, microphoneStopped):
		// 关机时麦克风可能比关机信号先停（systemd 同时通知整组进程）：稍等一下再判断（E-4）
		select {
		case <-ctx.Done():
			return errShutdown
		case <-time.After(300 * time.Millisecond):
		}
		s.logger.Warn("麦克风停了：重新找设备")
		return microphoneStopped
	case err != nil:
		s.logger.Error("对话运行出错：重来", "err", err)
		select { // 避免同一个错误转圈刷日志
		case <-ctx.Done():
			return errShutdown
		case <-time.After(s.timing.BackoffMin):
		}
		return err
	}
	return errShutdown // Run 正常返回 = 键盘输入结束等，按关机处理
}

// waitForMicrophone 等到有麦克风为止（没有就待机）；期间服务端断了就返回 errBrainLost。
func (s *Supervisor) waitForMicrophone(ctx context.Context, conn Connection) (Devices, error) {
	logged := false
	for {
		devices := s.scan()
		if devices.HasMicrophone {
			if logged {
				s.logger.Info("麦克风回来了", "device", devices.Microphone.Name)
			}
			return devices, nil
		}
		if !logged {
			s.logger.Warn("没有麦克风：待机，等设备接上")
			logged = true
		}
		select {
		case <-ctx.Done():
			return Devices{}, errShutdown
		case <-conn.Done():
			return Devices{}, errBrainLost
		case <-time.After(s.timing.Poll):
		}
	}
}

// watch 在对话运行期间看着连接和设备：服务端断了就结束；该换麦克风时（不在回答中）结束、重来；
// 音箱、摄像头有无变化时告诉服务端（没有音箱就不合成语音；没有摄像头就不给模型看画面的工具）。
func (s *Supervisor) watch(ctx context.Context, stop context.CancelCauseFunc, conn Connection, session Session, current Devices) {
	ticker := time.NewTicker(s.timing.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			stop(errBrainLost)
			return
		case <-ticker.C:
		}
		latest := s.scan()
		if latest.HasMicrophone && latest.Microphone.ID != current.Microphone.ID && !session.Busy() {
			s.logger.Info("换麦克风", "from", current.Microphone.Name, "to", latest.Microphone.Name,
				"why", latest.Microphone.Reason)
			stop(errSwitchDevice)
			return
		}
		changed := false
		if latest.HasSpeaker != current.HasSpeaker || latest.Speaker.Name != current.Speaker.Name {
			current.HasSpeaker, current.Speaker = latest.HasSpeaker, latest.Speaker
			if current.HasSpeaker {
				s.logger.Info("🔊 音箱", "device", current.Speaker.Name, "why", current.Speaker.Reason)
			} else {
				s.logger.Warn("没有音箱：照常听，回答只写日志")
			}
			changed = true
		}
		if latest.Camera != current.Camera { // 摄像头自己会记日志（只在状态变化时），这里只告诉服务端
			current.Camera = latest.Camera
			changed = true
		}
		if changed {
			if err := conn.Send(ctx, report(current)); err != nil {
				s.logger.Warn("告诉服务端设备变化失败", "err", err)
			}
		}
	}
}

// playTone 用本地提示音说一句（没有音箱就只写日志）。
func (s *Supervisor) playTone(ctx context.Context, tone realtimeproto.PCMAudio, why string) {
	s.logger.Info("播提示音", "why", why, "duration", tone.Duration().Round(time.Millisecond))
	if s.parts.ToneSink == nil || len(tone.Data) == 0 || !s.scan().HasSpeaker {
		s.logger.Warn("没有音箱：提示音只写日志", "why", why)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, tone.Duration()+5*time.Second)
	defer cancel()
	stream, err := s.parts.ToneSink.OpenStream(ctx, tone.Format)
	if err != nil {
		s.logger.Warn("播提示音失败", "err", err)
		return
	}
	if err := stream.Write(tone.Data); err == nil {
		err = stream.Drain()
	}
	if err != nil {
		_ = stream.Abort()
		s.logger.Warn("播提示音失败", "err", err)
	}
}

// scan 列一次设备、按规则选出麦克风和音箱（列不出来、选不出来的就标成没有）。
func (s *Supervisor) scan() Devices {
	var devices Devices
	if s.parts.CameraName != nil {
		devices.Camera = s.parts.CameraName()
	}
	if microphones, err := s.parts.Catalog.Microphones(); err == nil {
		devices.Microphone, err = ChooseMicrophone(microphones, s.parts.MicrophoneOverride)
		devices.HasMicrophone = err == nil
	}
	if speakers, err := s.parts.Catalog.Speakers(); err == nil {
		devices.Speaker, err = ChooseSpeaker(speakers, s.parts.SpeakerOverride)
		devices.HasSpeaker = err == nil
	}
	return devices
}

// report 把设备情况转成发给服务端的事件。
func report(devices Devices) realtimeproto.HaroTerminalDevices {
	event := realtimeproto.HaroTerminalDevices{Camera: devices.Camera}
	if devices.HasMicrophone {
		event.Microphone = devices.Microphone.Name
	}
	if devices.HasSpeaker {
		event.Speaker = devices.Speaker.Name
	}
	return event
}

func isDone(conn Connection) bool {
	select {
	case <-conn.Done():
		return true
	default:
		return false
	}
}
