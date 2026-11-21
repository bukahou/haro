// Package terminal 是终端的看守：按状态机把终端一直跑下去，除非收到关机信号，否则不退出。
//
//	连服务端（断了就按翻倍的间隔重连）
//	  → 等麦克风（没有、或指定的那个不在，就待机，按翻倍的间隔再查）
//	  → 运行对话（assistant）；同时每隔 Poll 按选择规则重新选一次设备
//
// 运行中任何一处出错（服务端断了、麦克风没了、该换麦克风了），都关掉这条连接、退回「连服务端」重来。
// 服务端断开超过 OfflineNotice，用本地提示音说一声；恢复时再说一声（断开时终端听不出唤醒词，只能主动说）。
// 协议版本和服务端不兼容时重连没用：提示一次，之后隔很久才再试，等人把两端升到同一版本。
// 各个时间的默认值见 DefaultTiming。
package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Connection 是一条到服务端的连接。
type Connection interface {
	assistant.BrainSession
	Done() <-chan struct{} // 连接断了时关闭
	Close() error
	// DeviceTools 是这条连接上的 MCP 通道：终端经它把设备能力（拍一帧等）提供给服务端。
	DeviceTools() mcp.Transport
}

// SelectedDevices 是某一刻按规则选出的设备（麦克风、音箱有没有、选了哪个，摄像头能不能用）。
type SelectedDevices struct {
	Microphone      Choice
	HasMicrophone   bool
	WhyNoMicrophone error // 没有麦克风的原因（列不出来、一个都没有、指定的那个不在）；有麦克风时为 nil
	Speaker         Choice
	HasSpeaker      bool
	WhyNoSpeaker    error  // 没有音箱的原因；有音箱时为 nil
	Camera          string // 现在能用的摄像头名字；空 = 没有
}

// Session 是一次对话运行（assistant.Assistant 满足它）。
type Session interface {
	Run(ctx context.Context) error // 只在 ctx 取消（关机、看守要它结束）或出错时返回
	Busy() bool
}

// SupervisorDependencies 是看守用到的各段能力，由 cmd/haro 组装注入。
type SupervisorDependencies struct {
	Dial               func(ctx context.Context) (Connection, error)
	Catalog            DeviceCatalog                                          // 列本机设备（平台层实现）；看守每 Poll 按规则选一次
	MicrophoneOverride string                                                 // 配置指定的麦克风 ID（HARO_AUDIO_SOURCE）；空 = 按规则选
	SpeakerOverride    string                                                 // 配置指定的音箱 ID（HARO_AUDIO_SINK）；空 = 跟系统默认
	CameraName         func() string                                          // 现在能用的摄像头名字；空 = 没有（nil = 这台终端不用摄像头）
	Tools              DeviceTools                                            // 经 MCP 提供给服务端的设备能力；nil = 不提供
	NewSession         func(conn Connection, devices SelectedDevices) Session // 用这条连接和这组设备创建一次对话运行
	ToneSink           assistant.AudioSink                                    // 播提示音用（跟系统默认）
	Offline            realtimeproto.PCMAudio                                 // 「我和服务端断开了……」
	Online             realtimeproto.PCMAudio                                 // 「我回来了」
}

// Timing 是看守的各个时间。
type Timing struct {
	OfflineNotice time.Duration // 服务端断开这么久才提示（躲开 make deploy 这种几秒的重启）
	Poll          time.Duration // 运行中多久查一次设备；待机等麦克风时从这个间隔开始翻倍
	BackoffMin    time.Duration // 重连服务端的间隔：从这么久开始翻倍
	BackoffMax    time.Duration // 重试间隔的上限（重连服务端、待机等麦克风都不超过它）
	// IncompatibleRetry 是协议版本不兼容时多久再试一次（要等人升级，频繁重试没用）；零值 = DefaultTiming 的
	IncompatibleRetry time.Duration
	// MicrophoneStopGrace：麦克风流自己停了以后先等这么久再判断是不是麦克风没了。定值：systemd 停服务时同时通知整组进程，
	// 麦克风可能比关机信号先停，等一下让关机信号先到，不把关机当成麦克风没了去重新找设备。零值 = DefaultTiming 的
	MicrophoneStopGrace time.Duration
	// ToneSlack：播一段提示音最多等「提示音的时长 + 这么久」（打开设备、等播完），和对话里播提示音用同一个值
	// （assistant.Timing.ToneSlack）。零值 = DefaultTiming 的
	ToneSlack time.Duration
}

// DefaultTiming 是看守的默认时间。重试间隔上限 5 秒：服务端重启（make deploy、集群里换新版本）几秒就回来，
// 上限再长，它回来以后终端还要白等十几秒才去连。
var DefaultTiming = Timing{OfflineNotice: 10 * time.Second, Poll: 2 * time.Second,
	BackoffMin: time.Second, BackoffMax: 5 * time.Second, IncompatibleRetry: 5 * time.Minute,
	MicrophoneStopGrace: 300 * time.Millisecond, ToneSlack: assistant.DefaultTiming.ToneSlack}

// Supervisor 是终端的看守。
type Supervisor struct {
	deps   SupervisorDependencies
	timing Timing
	logger *slog.Logger
}

// NewSupervisor 创建看守。
func NewSupervisor(deps SupervisorDependencies, timing Timing, logger *slog.Logger) *Supervisor {
	timing.IncompatibleRetry = cmp.Or(timing.IncompatibleRetry, DefaultTiming.IncompatibleRetry)
	timing.MicrophoneStopGrace = cmp.Or(timing.MicrophoneStopGrace, DefaultTiming.MicrophoneStopGrace)
	timing.ToneSlack = cmp.Or(timing.ToneSlack, DefaultTiming.ToneSlack)
	return &Supervisor{deps: deps, timing: timing, logger: logger}
}

// 一次运行为什么结束。
var (
	errBrainLost      = errors.New("和服务端的连接断了")
	errSwitchDevice   = errors.New("该换麦克风了")
	errShutdown       = errors.New("关机")
	errRunFailed      = errors.New("对话运行出错") // 例如设备在列表里、却打不开（被别的程序独占、权限）
	errRunEnded       = errors.New("对话运行没出错就结束了")
	microphoneStopped = assistant.ErrMicrophoneStopped
	// errDeviceToolsEnded：设备能力（MCP）的会话自己结束了，连接还在
	errDeviceToolsEnded = errors.New("设备能力（MCP）的会话结束了")
)

// offlineState 记服务端从什么时候连不上、有没有提示过。
type offlineState struct {
	since     time.Time
	announced bool
}

// Run 一直运行，直到 ctx 取消（关机信号）；正常关机返回 nil。
//
// 对话运行出错（例如设备在列表里、却打不开）时按翻倍的间隔重来（从 BackoffMin 到 BackoffMax），
// 不每隔固定时间就重连服务端、刷一条日志；中途因为别的原因（断线、换麦克风）结束过一次，间隔就从头算。
func (s *Supervisor) Run(ctx context.Context) error {
	runFailures := retrypolicy.NewBuilder[any]().
		HandleErrors(errRunFailed).
		WithBackoff(s.timing.BackoffMin, max(s.timing.BackoffMin, s.timing.BackoffMax)).
		WithMaxRetries(-1).
		OnRetryScheduled(func(event failsafe.ExecutionScheduledEvent[any]) {
			s.logger.Error("对话运行出错：重来", "err", event.LastError(), "retry_in", event.Delay)
		}).
		Build()
	offline := offlineState{since: time.Now()}
	for {
		reason := failsafe.With(runFailures).WithContext(ctx).Run(func() error { return s.runOnce(ctx, &offline) })
		switch {
		case ctx.Err() != nil || errors.Is(reason, errShutdown):
			return nil
		case errors.Is(reason, errBrainLost):
			s.logger.Warn("和服务端的连接断了：重连")
			offline = offlineState{since: time.Now()}
		}
	}
}

// runOnce 连上服务端，用这条连接运行到该结束为止；返回结束的原因。
func (s *Supervisor) runOnce(ctx context.Context, offline *offlineState) error {
	conn, err := s.connect(ctx, offline)
	if err != nil {
		return errShutdown // 只有关机才会连不上而返回
	}
	if offline.announced {
		s.playTone(ctx, s.deps.Online, "服务端恢复了")
	}
	*offline = offlineState{}
	defer conn.Close()
	return s.serve(ctx, conn)
}

// connect 连服务端，连不上就重试（间隔见 reconnectDelay），直到连上或关机；断开超过 OfflineNotice、
// 或协议版本不兼容时提示一次。
func (s *Supervisor) connect(ctx context.Context, offline *offlineState) (Connection, error) {
	reconnect := retrypolicy.NewBuilder[Connection]().
		WithMaxRetries(-1).
		WithDelayFunc(func(exec failsafe.ExecutionAttempt[Connection]) time.Duration {
			return s.reconnectDelay(exec.LastError(), exec.Retries(), offline)
		}).
		OnRetryScheduled(func(event failsafe.ExecutionScheduledEvent[Connection]) {
			if errors.Is(event.LastError(), realtimeproto.ErrIncompatibleProtocol) {
				s.logger.Error("和服务端的协议版本不兼容：把终端和服务端升到同一版本", "err", event.LastError(), "retry_in", event.Delay)
				return
			}
			s.logger.Warn("连不上服务端", "err", event.LastError(), "retry_in", event.Delay)
		}).
		Build()
	return failsafe.With(reconnect).WithContext(ctx).Get(func() (Connection, error) {
		conn, err := s.deps.Dial(ctx)
		if err == nil || ctx.Err() != nil {
			return conn, err
		}
		if offline.since.IsZero() { // 不是因为断线才来重连的（例如刚换完麦克风）：从现在开始算
			offline.since = time.Now()
		}
		switch {
		case offline.announced:
		case errors.Is(err, realtimeproto.ErrIncompatibleProtocol):
			offline.announced = true
			s.playTone(ctx, s.deps.Offline, "协议版本不兼容")
		case time.Since(offline.since) >= s.timing.OfflineNotice:
			offline.announced = true
			s.playTone(ctx, s.deps.Offline, "服务端断开超过提示时间")
		}
		return nil, err
	})
}

// reconnectDelay 是下一次重连服务端前等多久（retries = 已经重试过几次）：
//   - 协议版本不兼容：隔 IncompatibleRetry（要等人把两端升到同一版本，频繁重试没用）；
//   - 其余从 BackoffMin 翻倍，最长 BackoffMax；
//   - 还没提示断开时不越过提示时间：到提示时间先再连一次 —— 服务端可能正好在两次重试之间起来了
//     （make deploy 时服务端启动要好几秒，而间隔在翻倍），还连不上才提示，躲开这类重启。
func (s *Supervisor) reconnectDelay(lastErr error, retries int, offline *offlineState) time.Duration {
	if errors.Is(lastErr, realtimeproto.ErrIncompatibleProtocol) {
		return s.timing.IncompatibleRetry
	}
	delay := min(s.timing.BackoffMin<<min(retries, 20), s.timing.BackoffMax)
	if !offline.announced {
		delay = min(delay, max(0, time.Until(offline.since.Add(s.timing.OfflineNotice))))
	}
	return delay
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
	session := s.deps.NewSession(conn, devices)
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if s.deps.Tools != nil {
		s.deps.Tools.SetCameraAvailable(devices.Camera != "")
		// 设备能力（MCP）跟这条连接同生共死：连接还在、会话却自己结束了时断开重连 —— MCP 会话由服务端那头发起，
		// 只在这边重开对方不会重新初始化；留着这条连接，摄像头就一直用不了
		go func() {
			err := s.deps.Tools.Serve(runCtx, conn.DeviceTools())
			if runCtx.Err() == nil && !isDone(conn) {
				s.logger.Warn("设备能力（MCP）的会话结束了：断开重连，两边重新建会话", "err", err)
				cancel(errDeviceToolsEnded)
			}
		}()
	}
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
	case errors.Is(cause, errDeviceToolsEnded):
		return fmt.Errorf("%w: %w", errRunFailed, errDeviceToolsEnded) // 按翻倍的间隔重来：会话一连上就结束时不狂连
	case errors.Is(err, microphoneStopped):
		select { // 关机时麦克风可能比关机信号先停：稍等一下再判断（见 Timing.MicrophoneStopGrace）
		case <-ctx.Done():
			return errShutdown
		case <-time.After(s.timing.MicrophoneStopGrace):
		}
		s.logger.Warn("麦克风停了：重新找设备")
		return microphoneStopped
	case err != nil:
		return fmt.Errorf("%w: %w", errRunFailed, err) // Run 按翻倍的间隔重来
	}
	// 对话运行不会没出错就自己结束（见 Session）：真这样了也按出错重来，不当成关机退出
	return fmt.Errorf("%w: %w", errRunFailed, errRunEnded)
}

// waitForMicrophone 等到有麦克风为止：没有（或指定的那个不在）就待机，按翻倍的间隔再查（从 Poll 开始，最长 BackoffMax）；
// 进入待机、麦克风回来各记一条日志。期间服务端断了返回 errBrainLost，关机返回 errShutdown。
func (s *Supervisor) waitForMicrophone(ctx context.Context, conn Connection) (SelectedDevices, error) {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // 待机期间服务端断了：不再等，回去重连
		select {
		case <-conn.Done():
			cancel()
		case <-waitCtx.Done():
		}
	}()
	standby := false
	policy := retrypolicy.NewBuilder[SelectedDevices]().
		WithBackoff(s.timing.Poll, max(s.timing.Poll, s.timing.BackoffMax)).
		WithMaxRetries(-1).
		OnRetryScheduled(func(event failsafe.ExecutionScheduledEvent[SelectedDevices]) {
			if !standby {
				standby = true
				s.logger.Warn("没有麦克风：待机，等设备接上", "why", event.LastError())
			}
		}).
		Build()
	devices, err := failsafe.With(policy).WithContext(waitCtx).Get(func() (SelectedDevices, error) {
		devices := s.scan()
		return devices, devices.WhyNoMicrophone
	})
	switch {
	case ctx.Err() != nil:
		return SelectedDevices{}, errShutdown
	case err != nil: // 重试不设次数上限：只有等待被取消（服务端断了）才会带着错误结束
		return SelectedDevices{}, errBrainLost
	}
	if standby {
		s.logger.Info("麦克风回来了", "device", devices.Microphone.Name)
	}
	return devices, nil
}

// watch 在对话运行期间看着连接和设备：服务端断了就结束；该换麦克风时（不在回答中）结束、重来；
// 音箱有无变化时告诉服务端（没有音箱就不合成语音）；摄像头有无变化时增删「拍一帧」工具（服务端随之给不给模型看画面）。
func (s *Supervisor) watch(ctx context.Context, stop context.CancelCauseFunc, conn Connection, session Session, current SelectedDevices) {
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
				s.logger.Warn("没有音箱：照常听，回答只写日志", "why", latest.WhyNoSpeaker)
			}
			changed = true
		}
		if latest.Camera != current.Camera {
			current.Camera = latest.Camera
			if current.Camera != "" {
				s.logger.Info("📷 摄像头能用了", "device", current.Camera)
			} else {
				s.logger.Warn("📷 摄像头现在用不了")
			}
			if s.deps.Tools != nil {
				s.deps.Tools.SetCameraAvailable(current.Camera != "")
			}
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
	if s.deps.ToneSink == nil || len(tone.Data) == 0 || !s.scan().HasSpeaker {
		s.logger.Warn("没有音箱：提示音只写日志", "why", why)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, tone.Duration()+s.timing.ToneSlack)
	defer cancel()
	stream, err := s.deps.ToneSink.OpenStream(ctx, tone.Format)
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

// scan 列一次设备、按规则选出麦克风和音箱（列不出来、选不出来的就标成没有，原因记在 WhyNo…）。
func (s *Supervisor) scan() SelectedDevices {
	var devices SelectedDevices
	if s.deps.CameraName != nil {
		devices.Camera = s.deps.CameraName()
	}
	microphones, err := s.deps.Catalog.Microphones()
	if err == nil {
		devices.Microphone, err = ChooseMicrophone(microphones, s.deps.MicrophoneOverride)
	}
	devices.HasMicrophone, devices.WhyNoMicrophone = err == nil, err
	speakers, err := s.deps.Catalog.Speakers()
	if err == nil {
		devices.Speaker, err = ChooseSpeaker(speakers, s.deps.SpeakerOverride)
	}
	devices.HasSpeaker, devices.WhyNoSpeaker = err == nil, err
	return devices
}

// report 把设备情况转成发给服务端的事件。
func report(devices SelectedDevices) realtimeproto.HaroTerminalDevices {
	var event realtimeproto.HaroTerminalDevices
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
