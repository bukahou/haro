package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/mcpbridge"
	"github.com/bukahou/haro/internal/realtimeproto"
)

var testTiming = Timing{OfflineNotice: 60 * time.Millisecond, Poll: 10 * time.Millisecond,
	BackoffMin: 10 * time.Millisecond, BackoffMax: 20 * time.Millisecond}

// fakeConn 是一条假的服务端连接：记下发过的设备事件；cut() 模拟断线。
type fakeConn struct {
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	devices []realtimeproto.HaroTerminalDevices
	tools   *mcpbridge.Transport
}

func newFakeConn() *fakeConn {
	return &fakeConn{done: make(chan struct{}),
		tools: mcpbridge.NewTransport(func(context.Context, json.RawMessage) error { return nil })}
}

func (c *fakeConn) DeviceTools() mcp.Transport { return c.tools }

// fakeTools 记下「拍一帧」的增删；Serve 一直跑到连接结束（endFirst 设了时第一次马上结束：连接还在，会话自己结束了）。
type fakeTools struct {
	mu        sync.Mutex
	available []bool
	serves    atomic.Int32
	endFirst  atomic.Bool
}

func (f *fakeTools) Serve(ctx context.Context, _ mcp.Transport) error {
	if f.serves.Add(1) == 1 && f.endFirst.Load() {
		return errors.New("MCP 会话结束了")
	}
	<-ctx.Done()
	return nil
}

func (f *fakeTools) SetCameraAvailable(available bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.available = append(f.available, available)
}

// lastCamera 是最后一次设的摄像头能不能用。
func (f *fakeTools) lastCamera() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.available) > 0 && f.available[len(f.available)-1]
}

func (c *fakeConn) Send(_ context.Context, event realtimeproto.ClientEvent) error {
	if isDone(c) {
		return errors.New("断了")
	}
	if devices, ok := event.(realtimeproto.HaroTerminalDevices); ok {
		c.mu.Lock()
		c.devices = append(c.devices, devices)
		c.mu.Unlock()
	}
	return nil
}
func (c *fakeConn) Events() <-chan realtimeproto.ServerEvent { return nil }
func (c *fakeConn) Done() <-chan struct{}                    { return c.done }
func (c *fakeConn) Close() error                             { c.cut(); return nil }
func (c *fakeConn) cut()                                     { c.once.Do(func() { close(c.done) }) }
func (c *fakeConn) reports() []realtimeproto.HaroTerminalDevices {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]realtimeproto.HaroTerminalDevices(nil), c.devices...)
}

// fakeSession 一直运行到被取消（或 fail 里来了错误）；openErr 有值时一开始就以它结束（设备打不开）。
type fakeSession struct {
	fail    chan error
	openErr func() error
	running *atomic.Int32 // 正在运行（没有一开始就出错）的次数
}

func (s fakeSession) Run(ctx context.Context) error {
	if err := s.openErr(); err != nil {
		return err
	}
	s.running.Add(1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-s.fail:
		return err
	}
}
func (fakeSession) Busy() bool { return false }

// fakeSink 记下播过的提示音（按时长区分）。
type fakeSink struct {
	mu     sync.Mutex
	played []time.Duration
}

func (s *fakeSink) OpenStream(context.Context, realtimeproto.AudioFormat) (assistant.AudioStream, error) {
	return &fakeStream{sink: s}, nil
}
func (s *fakeSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.played) }

type fakeStream struct {
	sink *fakeSink
	n    int
}

func (f *fakeStream) Write(pcm []byte) error { f.n += len(pcm); return nil }
func (f *fakeStream) Drain() error {
	f.sink.mu.Lock()
	f.sink.played = append(f.sink.played, time.Duration(f.n))
	f.sink.mu.Unlock()
	return nil
}
func (f *fakeStream) Abort() error          { return nil }
func (f *fakeStream) Played() time.Duration { return 0 }

// rig 是一套可操控的假部件。
type rig struct {
	brainUp      atomic.Bool
	upAfterDials atomic.Int32 // 服务端在第几次连接失败之后起来（0 = 不自己起来）
	incompatible atomic.Bool  // 服务端的协议版本和终端不兼容
	dials        atomic.Int32 // 连了几次服务端
	microphone   atomic.Value // string：列出的那一个麦克风的 ID；空 = 没有麦克风
	micOverride  string       // 配置指定的麦克风 ID
	speaker      atomic.Bool
	camera       atomic.Value // string：空 = 摄像头现在不可用
	mu           sync.Mutex
	dialedAt     []time.Time // 每次连服务端的时刻
	micScannedAt []time.Time // 每次列麦克风的时刻
	conns        []*fakeConn
	sessions     []SelectedDevices
	fail         chan error
	openErr      error        // 设了：每次对话运行一开始就以它结束（设备在列表里、却打不开）
	running      atomic.Int32 // 对话正常运行起来的次数
	sink         fakeSink
	tools        fakeTools
}

func (r *rig) setOpenError(err error) { r.mu.Lock(); defer r.mu.Unlock(); r.openErr = err }
func (r *rig) openError() error       { r.mu.Lock(); defer r.mu.Unlock(); return r.openErr }

func newRig() *rig {
	r := &rig{fail: make(chan error, 1)}
	r.brainUp.Store(true)
	r.microphone.Store("usb")
	r.speaker.Store(true)
	r.camera.Store("EMEET")
	return r
}

func (r *rig) dependencies() SupervisorDependencies {
	return SupervisorDependencies{
		Dial: func(context.Context) (Connection, error) {
			n := r.dials.Add(1)
			r.mu.Lock()
			r.dialedAt = append(r.dialedAt, time.Now())
			r.mu.Unlock()
			if r.incompatible.Load() {
				return nil, fmt.Errorf("%w：服务端只支持更新的版本", realtimeproto.ErrIncompatibleProtocol)
			}
			if !r.brainUp.Load() {
				if n == r.upAfterDials.Load() { // 这次还连不上，下一次就起来了
					r.brainUp.Store(true)
				}
				return nil, errors.New("连不上")
			}
			conn := newFakeConn()
			r.mu.Lock()
			r.conns = append(r.conns, conn)
			r.mu.Unlock()
			return conn, nil
		},
		Catalog:            rigCatalog{r},
		MicrophoneOverride: r.micOverride,
		CameraName:         func() string { return r.camera.Load().(string) },
		Tools:              &r.tools,
		NewSession: func(_ Connection, devices SelectedDevices) Session {
			r.mu.Lock()
			r.sessions = append(r.sessions, devices)
			r.mu.Unlock()
			return fakeSession{fail: r.fail, openErr: r.openError, running: &r.running}
		},
		ToneSink: &r.sink,
		Offline:  realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: make([]byte, 100)},
		Online:   realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: make([]byte, 20)},
	}
}

// rigCatalog 是假的设备目录：麦克风 / 音箱有没有由 rig 控制。
type rigCatalog struct{ r *rig }

func (c rigCatalog) Microphones() ([]Device, error) {
	c.r.mu.Lock()
	c.r.micScannedAt = append(c.r.micScannedAt, time.Now())
	c.r.mu.Unlock()
	if mic := c.r.microphone.Load().(string); mic != "" {
		return []Device{{ID: mic, Name: mic, Default: true, Bus: BusUSB}}, nil
	}
	return nil, nil
}
func (c rigCatalog) Speakers() ([]Device, error) {
	if c.r.speaker.Load() {
		return []Device{{ID: "speaker", Name: "音箱", Default: true}}, nil
	}
	return nil, nil
}
func (rigCatalog) Cameras(context.Context) ([]Device, error) { return nil, nil }

func (r *rig) lastConn() *fakeConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns[len(r.conns)-1]
}
func (r *rig) sessionCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.sessions) }
func (r *rig) micScans() int     { r.mu.Lock(); defer r.mu.Unlock(); return len(r.micScannedAt) }

// gap 是第 i 次和第 i+1 次之间隔了多久（从 0 数）。
func gap(times []time.Time, i int) time.Duration { return times[i+1].Sub(times[i]) }

func (r *rig) times(which *[]time.Time) []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), *which...)
}
func (r *rig) lastSession() SelectedDevices {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[len(r.sessions)-1]
}

func startSupervisor(t *testing.T, r *rig) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewSupervisor(r.dependencies(), testTiming, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	}()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(time.Second):
			t.Fatal("关机后看守没有退出")
			return nil
		}
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 服务端断开：短时间内恢复不出声；超过提示时间就说「断开了」，恢复时说「回来了」；关机正常退出。
func TestSupervisorReconnectsAndAnnouncesLongOutage(t *testing.T) {
	r := newRig()
	stop := startSupervisor(t, r)
	eventually(t, "第一次运行", func() bool { return r.sessionCount() == 1 })

	r.lastConn().cut() // 短暂断开：马上能重连
	eventually(t, "重连后再运行", func() bool { return r.sessionCount() == 2 })
	if r.sink.count() != 0 {
		t.Fatalf("短暂断开不该出声：%d", r.sink.count())
	}

	r.brainUp.Store(false)
	r.lastConn().cut()
	eventually(t, "提示「断开了」", func() bool { return r.sink.count() == 1 })
	r.brainUp.Store(true)
	eventually(t, "恢复后运行并提示「回来了」", func() bool { return r.sessionCount() == 3 && r.sink.count() == 2 })
	if err := stop(); err != nil {
		t.Errorf("关机应正常返回：%v", err)
	}
}

// 服务端在两次重试之间起来（make deploy：服务端启动要几秒，重试间隔在翻倍）：到提示时间先再连一次，连上就不出声。
func TestSupervisorRetriesBeforeAnnouncing(t *testing.T) {
	r := newRig()
	r.brainUp.Store(false)
	r.upAfterDials.Store(2) // 第二次重试（200 ms）失败之后起来：下一次按间隔要到 600 ms，提示时间 300 ms 在这之前
	timing := Timing{OfflineNotice: 300 * time.Millisecond, Poll: 10 * time.Millisecond,
		BackoffMin: 200 * time.Millisecond, BackoffMax: time.Second} // 重试在 0、200、600 ms：提示时间 300 ms 落在中间
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewSupervisor(r.dependencies(), timing, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	eventually(t, "连上并运行", func() bool { return r.sessionCount() == 1 })
	// 提示音在连上之前播（connect 返回前），所以连上时还没出声就是没出声
	if n := r.sink.count(); n != 0 {
		t.Fatalf("到提示时间时服务端已经起来了，不该出声：%d", n)
	}
	if n := r.dials.Load(); n != 3 {
		t.Errorf("应在提示时间先再连一次（第 3 次就连上）：连了 %d 次", n)
	}
}

// 没有麦克风就待机，接上后开始运行；麦克风停了（被拔掉）不退出，换成剩下的那个。
func TestSupervisorWaitsForAndSwitchesMicrophone(t *testing.T) {
	r := newRig()
	r.microphone.Store("")
	stop := startSupervisor(t, r)
	eventually(t, "待机时查了几次设备", func() bool { return r.micScans() >= 3 })
	if r.sessionCount() != 0 {
		t.Fatal("没有麦克风时不该开始运行")
	}
	r.microphone.Store("usb")
	eventually(t, "麦克风接上后运行", func() bool { return r.sessionCount() == 1 })

	r.microphone.Store("bluetooth") // 规则选出来的变了（例如 USB 被拔掉，只剩蓝牙）
	eventually(t, "换成新的麦克风", func() bool { return r.sessionCount() == 2 && r.lastSession().Microphone.ID == "bluetooth" })

	r.fail <- assistant.ErrMicrophoneStopped // 麦克风流自己停了
	eventually(t, "麦克风停了之后重来", func() bool { return r.sessionCount() == 3 })
	_ = stop()
}

// 对话运行没出错就结束了（不该发生）：按出错重来，不当成关机退出（只有关机信号才退出）。
func TestSupervisorRestartsWhenRunEndsWithoutError(t *testing.T) {
	r := newRig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- NewSupervisor(r.dependencies(), testTiming, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	}()
	eventually(t, "运行", func() bool { return r.sessionCount() == 1 })
	r.fail <- nil
	eventually(t, "没出错就结束了也重来", func() bool { return r.sessionCount() == 2 })
	select {
	case err := <-done:
		t.Fatalf("没收到关机信号不该退出：%v", err)
	default:
	}
}

// 麦克风流自己停了：先等 Timing.MicrophoneStopGrace（关机信号可能晚一步到）再去重新找设备。
func TestSupervisorWaitsMicrophoneStopGrace(t *testing.T) {
	r := newRig()
	timing := testTiming
	timing.MicrophoneStopGrace = 600 * time.Millisecond // 比默认的长：用的确实是传进来的值
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewSupervisor(r.dependencies(), timing, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	eventually(t, "运行", func() bool { return r.sessionCount() == 1 })
	stopped := time.Now()
	r.fail <- assistant.ErrMicrophoneStopped
	eventually(t, "麦克风停了之后重来", func() bool { return r.sessionCount() == 2 })
	if waited := time.Since(stopped); waited < timing.MicrophoneStopGrace {
		t.Errorf("应先等 %v 再重来，只等了 %v", timing.MicrophoneStopGrace, waited)
	}
}

// 音箱没了 / 回来了，都告诉服务端（没有音箱时服务端只发文字）。
func TestSupervisorReportsSpeakerChanges(t *testing.T) {
	r := newRig()
	stop := startSupervisor(t, r)
	eventually(t, "运行", func() bool { return r.sessionCount() == 1 })
	conn := r.lastConn()
	r.speaker.Store(false)
	eventually(t, "报告没有音箱", func() bool {
		reports := conn.reports()
		return len(reports) >= 2 && reports[len(reports)-1].Speaker == ""
	})
	r.speaker.Store(true)
	eventually(t, "报告音箱回来了", func() bool {
		reports := conn.reports()
		return reports[len(reports)-1].Speaker == "音箱"
	})
	if r.sessionCount() != 1 {
		t.Error("音箱变化不该重来")
	}
	_ = stop()
}

// 摄像头被拔掉 / 插回来，都告诉服务端（v4 第 5 步 b：没有摄像头时服务端不给模型看画面的工具），且不重来。
// 摄像头插拔：增删「拍一帧」工具（服务端随之给不给模型看画面），对话不重来。
func TestSupervisorOffersCameraToolWhileCameraIsAvailable(t *testing.T) {
	r := newRig()
	stop := startSupervisor(t, r)
	eventually(t, "运行", func() bool { return r.sessionCount() == 1 })
	eventually(t, "连接上跑起设备能力的会话", func() bool { return r.tools.serves.Load() == 1 })
	if !r.tools.lastCamera() {
		t.Fatal("一开始有摄像头：应提供「拍一帧」")
	}
	r.camera.Store("")
	eventually(t, "摄像头没了：去掉「拍一帧」", func() bool { return !r.tools.lastCamera() })
	r.camera.Store("EMEET")
	eventually(t, "摄像头回来了：加回「拍一帧」", r.tools.lastCamera)
	if r.sessionCount() != 1 || r.tools.serves.Load() != 1 {
		t.Error("摄像头变化不该重来")
	}
	_ = stop()
}

// 设备能力（MCP）的会话自己结束了而连接还在：关掉这条连接、重连（两边重新建 MCP 会话），不留着一条摄像头一直用不了的连接。
func TestSupervisorReconnectsWhenDeviceToolsSessionEnds(t *testing.T) {
	r := newRig()
	r.tools.endFirst.Store(true)
	stop := startSupervisor(t, r)
	eventually(t, "MCP 会话结束后断开重连、再跑起来", func() bool {
		return r.dials.Load() == 2 && r.tools.serves.Load() == 2 && r.sessionCount() == 2
	})
	r.mu.Lock()
	first := r.conns[0]
	r.mu.Unlock()
	select {
	case <-first.done:
	default:
		t.Error("MCP 会话结束的那条连接应被关掉")
	}
	_ = stop()
}

// 协议版本和服务端不兼容：重连没用。马上提示一次，之后隔 IncompatibleRetry 才再试（不按平常的间隔频繁重连）；
// 两端升到同一版本后照常连上，并提示「回来了」。
func TestSupervisorBacksOffOnIncompatibleProtocol(t *testing.T) {
	r := newRig()
	r.incompatible.Store(true)
	timing := testTiming
	timing.IncompatibleRetry = 400 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewSupervisor(r.dependencies(), timing, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	eventually(t, "提示一次", func() bool { return r.sink.count() == 1 })
	r.incompatible.Store(false)
	eventually(t, "隔一段时间再试，连上并运行", func() bool { return r.sessionCount() == 1 })
	eventually(t, "提示「回来了」", func() bool { return r.sink.count() == 2 })
	// 平常的重连间隔是 10–20 ms：不兼容之后的下一次要隔够 IncompatibleRetry
	if dials := r.times(&r.dialedAt); len(dials) != 2 || gap(dials, 0) < timing.IncompatibleRetry {
		t.Fatalf("不兼容时不该频繁重连：连了 %d 次，前两次间隔 %v", len(dials), gap(dials, 0))
	}
}

// 指定的麦克风不在（被拔掉、蓝牙断了、ID 写错了）：待机，不开始运行、不反复重连服务端；它接上后开始运行，用的就是它。
func TestSupervisorWaitsForConfiguredMicrophone(t *testing.T) {
	r := newRig()
	r.micOverride = "bluetooth" // 列出的是 usb，不是指定的这个
	stop := startSupervisor(t, r)
	eventually(t, "待机时查了几次设备", func() bool { return r.micScans() >= 3 })
	if r.sessionCount() != 0 {
		t.Fatal("指定的麦克风不在：不该开始运行")
	}
	if n := r.dials.Load(); n != 1 {
		t.Fatalf("待机时不该反复重连服务端：连了 %d 次", n)
	}
	r.microphone.Store("bluetooth")
	eventually(t, "指定的麦克风接上后运行", func() bool {
		return r.sessionCount() == 1 && r.lastSession().Microphone.ID == "bluetooth"
	})
	_ = stop()
}

// 待机时查设备的间隔从 Poll 开始翻倍，到 BackoffMax 为止（只查下限：计时器不会提前到，慢机器上也不会误报）。
func TestSupervisorStandbyBacksOff(t *testing.T) {
	r := newRig()
	r.microphone.Store("")
	timing := testTiming
	timing.Poll, timing.BackoffMax = 10*time.Millisecond, 40*time.Millisecond // 间隔 10、20、40、40……
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewSupervisor(r.dependencies(), timing, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	eventually(t, "待机时查了 5 次设备", func() bool { return r.micScans() >= 5 })
	scans := r.times(&r.micScannedAt)
	if gap(scans, 0) < timing.Poll || gap(scans, 2) < 4*timing.Poll || gap(scans, 3) < timing.BackoffMax {
		t.Errorf("待机时查设备的间隔应从 %v 翻倍到 %v：%v、%v、%v、%v", timing.Poll, timing.BackoffMax,
			gap(scans, 0), gap(scans, 1), gap(scans, 2), gap(scans, 3))
	}
}

// 设备在列表里、却打不开（被别的程序独占、权限）：对话运行一开始就出错 —— 按翻倍的间隔重来，到 BackoffMax 为止，
// 不每隔固定时间就重连服务端、刷日志（只查下限：计时器不会提前到）；能打开以后照常运行。
func TestSupervisorBacksOffWhenRunKeepsFailing(t *testing.T) {
	r := newRig()
	r.setOpenError(errors.New("设备被别的程序占着"))
	timing := testTiming
	timing.BackoffMin, timing.BackoffMax = 10*time.Millisecond, 40*time.Millisecond // 间隔 10、20、40、40……
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewSupervisor(r.dependencies(), timing, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	eventually(t, "重来了 5 次", func() bool { return r.dials.Load() >= 5 })
	dials := r.times(&r.dialedAt)
	if gap(dials, 0) < timing.BackoffMin || gap(dials, 2) < 4*timing.BackoffMin || gap(dials, 3) < timing.BackoffMax {
		t.Errorf("运行一直出错时，重来的间隔应从 %v 翻倍到 %v：%v、%v、%v、%v", timing.BackoffMin, timing.BackoffMax,
			gap(dials, 0), gap(dials, 1), gap(dials, 2), gap(dials, 3))
	}
	r.setOpenError(nil)
	eventually(t, "能打开以后照常运行", func() bool { return r.running.Load() == 1 })
}

// 重连的间隔：从 BackoffMin 翻倍到 BackoffMax；协议版本不兼容隔 IncompatibleRetry；还没提示断开时不越过提示时间。
func TestReconnectDelay(t *testing.T) {
	timing := Timing{OfflineNotice: 10 * time.Second, BackoffMin: time.Second, BackoffMax: 5 * time.Second, IncompatibleRetry: 5 * time.Minute}
	s := NewSupervisor(SupervisorDependencies{}, timing, slog.New(slog.NewTextHandler(io.Discard, nil)))
	announced := &offlineState{since: time.Now().Add(-time.Hour), announced: true}
	refused := errors.New("连不上")
	for retries, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second} {
		if got := s.reconnectDelay(refused, retries, announced); got != want {
			t.Errorf("第 %d 次重试前应等 %v：%v", retries, want, got)
		}
	}
	if got := s.reconnectDelay(fmt.Errorf("%w：要更新的版本", realtimeproto.ErrIncompatibleProtocol), 0, announced); got != timing.IncompatibleRetry {
		t.Errorf("协议版本不兼容应隔 %v：%v", timing.IncompatibleRetry, got)
	}
	nearNotice := &offlineState{since: time.Now().Add(-9 * time.Second)} // 再过 1 秒就到提示时间
	if got := s.reconnectDelay(refused, 3, nearNotice); got > time.Second {
		t.Errorf("还没提示时不越过提示时间（按翻倍该等 5 秒）：%v", got)
	}
	if got := s.reconnectDelay(refused, 50, announced); got != timing.BackoffMax {
		t.Errorf("重试很多次以后也只等 BackoffMax：%v", got)
	}
}
