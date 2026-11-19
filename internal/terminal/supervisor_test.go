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

// fakeTools 记下「拍一帧」的增删；Serve 一直跑到连接结束。
type fakeTools struct {
	mu        sync.Mutex
	available []bool
	serves    atomic.Int32
}

func (f *fakeTools) Serve(ctx context.Context, _ mcp.Transport) error {
	f.serves.Add(1)
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

// fakeSession 一直运行到被取消（或 fail 里来了错误）。
type fakeSession struct{ fail chan error }

func (s fakeSession) Run(ctx context.Context) error {
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
	incompatible atomic.Bool  // 服务端的协议版本和终端不兼容
	dials        atomic.Int32 // 连了几次服务端
	microphone   atomic.Value // string：空 = 没有麦克风
	speaker      atomic.Bool
	camera       atomic.Value // string：空 = 摄像头现在不可用
	mu           sync.Mutex
	conns        []*fakeConn
	sessions     []Devices
	fail         chan error
	sink         fakeSink
	tools        fakeTools
}

func newRig() *rig {
	r := &rig{fail: make(chan error, 1)}
	r.brainUp.Store(true)
	r.microphone.Store("usb")
	r.speaker.Store(true)
	r.camera.Store("EMEET")
	return r
}

func (r *rig) parts() Parts {
	return Parts{
		Dial: func(context.Context) (Connection, error) {
			r.dials.Add(1)
			if r.incompatible.Load() {
				return nil, fmt.Errorf("%w：服务端只支持更新的版本", realtimeproto.ErrIncompatibleProtocol)
			}
			if !r.brainUp.Load() {
				return nil, errors.New("连不上")
			}
			conn := newFakeConn()
			r.mu.Lock()
			r.conns = append(r.conns, conn)
			r.mu.Unlock()
			return conn, nil
		},
		Catalog:    rigCatalog{r},
		CameraName: func() string { return r.camera.Load().(string) },
		Tools:      &r.tools,
		NewSession: func(_ Connection, devices Devices) Session {
			r.mu.Lock()
			r.sessions = append(r.sessions, devices)
			r.mu.Unlock()
			return fakeSession{fail: r.fail}
		},
		ToneSink: &r.sink,
		Offline:  realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: make([]byte, 100)},
		Online:   realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: make([]byte, 20)},
	}
}

// rigCatalog 是假的设备目录：麦克风 / 音箱有没有由 rig 控制。
type rigCatalog struct{ r *rig }

func (c rigCatalog) Microphones() ([]Device, error) {
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
func (r *rig) lastSession() Devices {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[len(r.sessions)-1]
}

func startSupervisor(t *testing.T, r *rig) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewSupervisor(r.parts(), testTiming, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
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
	timing := Timing{OfflineNotice: 300 * time.Millisecond, Poll: 10 * time.Millisecond,
		BackoffMin: 200 * time.Millisecond, BackoffMax: time.Second} // 重试在 0、200、600 ms：提示时间 300 ms 落在中间
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewSupervisor(r.parts(), timing, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	time.Sleep(250 * time.Millisecond)
	r.brainUp.Store(true) // 第二次重试（200 ms）之后、提示时间（300 ms）之前起来
	eventually(t, "连上并运行", func() bool { return r.sessionCount() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := r.sink.count(); n != 0 {
		t.Fatalf("到提示时间时服务端已经起来了，不该出声：%d", n)
	}
}

// 没有麦克风就待机，接上后开始运行；麦克风停了（被拔掉）不退出，换成剩下的那个。
func TestSupervisorWaitsForAndSwitchesMicrophone(t *testing.T) {
	r := newRig()
	r.microphone.Store("")
	stop := startSupervisor(t, r)
	time.Sleep(50 * time.Millisecond)
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

// 协议版本和服务端不兼容：重连没用。马上提示一次，之后隔 IncompatibleRetry 才再试（不按平常的间隔频繁重连）；
// 两端升到同一版本后照常连上，并提示「回来了」。
func TestSupervisorBacksOffOnIncompatibleProtocol(t *testing.T) {
	r := newRig()
	r.incompatible.Store(true)
	timing := testTiming
	timing.IncompatibleRetry = 400 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewSupervisor(r.parts(), timing, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	eventually(t, "提示一次", func() bool { return r.sink.count() == 1 })
	time.Sleep(200 * time.Millisecond) // 平常的重连间隔是 10–20 ms，这段时间里会连十几次
	if n := r.dials.Load(); n != 1 {
		t.Fatalf("不兼容时不该频繁重连：连了 %d 次", n)
	}
	r.incompatible.Store(false)
	eventually(t, "隔一段时间再试，连上并运行", func() bool { return r.sessionCount() == 1 })
	eventually(t, "提示「回来了」", func() bool { return r.sink.count() == 2 })
}
