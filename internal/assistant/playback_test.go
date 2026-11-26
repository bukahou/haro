package assistant

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// scriptedBrain：事件由测试推送；收到 response.cancel 时按真实服务端的样子补发（先一个用户开口的事件，再 response.done）。
type scriptedBrain struct {
	events chan realtimeproto.ServerEvent
	mu     sync.Mutex
	sent   []realtimeproto.ClientEvent
}

func (b *scriptedBrain) Send(_ context.Context, event realtimeproto.ClientEvent) error {
	b.mu.Lock()
	b.sent = append(b.sent, event)
	b.mu.Unlock()
	if _, ok := event.(realtimeproto.ResponseCancel); ok {
		b.events <- realtimeproto.InputAudioBufferSpeechStarted{ItemID: "item_9"}
		b.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}
	}
	return nil
}
func (b *scriptedBrain) Events() <-chan realtimeproto.ServerEvent { return b.events }
func (b *scriptedBrain) sentTypes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var types []string
	for _, event := range b.sent {
		types = append(types, event.Type())
	}
	return types
}

// slowSink：Drain 一直等到被 Abort（模拟声音还在播）；release 关闭后 Drain 正常返回（播完）。opened 在打开播放流时关闭。
type slowSink struct {
	release    chan struct{}
	aborted    chan struct{}
	opened     chan struct{}
	once       sync.Once
	openedOnce sync.Once
}

func (s *slowSink) OpenStream(context.Context, realtimeproto.AudioFormat) (AudioStream, error) {
	s.openedOnce.Do(func() { close(s.opened) })
	return s, nil
}
func (s *slowSink) Write([]byte) error { return nil }
func (s *slowSink) Drain() error {
	select {
	case <-s.release:
		return nil
	case <-s.aborted:
		return errors.New("aborted")
	}
}
func (s *slowSink) Abort() error          { s.once.Do(func() { close(s.aborted) }); return nil }
func (s *slowSink) Played() time.Duration { return 1234 * time.Millisecond }
func (s *slowSink) wasAborted() bool {
	select {
	case <-s.aborted:
		return true
	default:
		return false
	}
}
func newSlowSink() *slowSink {
	return &slowSink{release: make(chan struct{}), aborted: make(chan struct{}), opened: make(chan struct{})}
}

var chunk = realtimeproto.ResponseOutputAudioDelta{Audio: realtimeproto.PCMAudio{
	Format: realtimeproto.AudioFormat{SampleRate: 32000, Channels: 1}, Data: make([]byte, 6400)}}

func newBargeInAssistant(brain *scriptedBrain, sink *slowSink) *Assistant {
	return NewAssistant(Dependencies{Brain: brain, Speaker: sink}, Options{BargeIn: true,
		WakeWord: WakeWordOptions{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func noSpan() trace.Span { return trace.SpanFromContext(context.Background()) }

// 回答还在生成时喊唤醒词：立即停播，告诉服务端听到了多少，再取消；取消期间用户开口的事件留给主循环。
func TestBargeInWhileGenerating(t *testing.T) {
	brain, sink := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	a := newBargeInAssistant(brain, sink)
	brain.events <- chunk
	brain.events <- chunk
	brain.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗"}

	_, err := a.playResponse(context.Background(), noSpan(), "item_1", time.Now())
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("应返回被打断，得到 %v", err)
	}
	if !sink.wasAborted() {
		t.Error("应立即停止播放")
	}
	sent := brain.sentTypes()
	if len(sent) != 2 || sent[0] != "conversation.item.truncate" || sent[1] != "response.cancel" {
		t.Errorf("应先截断再取消：%v", sent)
	}
	if truncate := brain.sent[0].(realtimeproto.ConversationItemTruncate); truncate.AudioEndMs != 1234 {
		t.Errorf("截断点应是已播时长：%+v", truncate)
	}
	select {
	case event := <-a.replay:
		if _, ok := event.(realtimeproto.InputAudioBufferSpeechStarted); !ok {
			t.Errorf("留给主循环的应是用户开口：%v", event)
		}
	default:
		t.Error("取消期间的其他事件应留给主循环")
	}
}

// 回答已经生成完、声音还在播：只截断，不用取消（回答已经结束了）。
func TestBargeInWhileDraining(t *testing.T) {
	brain, sink := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	a := newBargeInAssistant(brain, sink)
	brain.events <- chunk
	brain.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	go func() {
		time.Sleep(50 * time.Millisecond) // 让它先进入「等播完」
		brain.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗"}
	}()
	_, err := a.playResponse(context.Background(), noSpan(), "item_1", time.Now())
	if !errors.Is(err, errInterrupted) || !sink.wasAborted() {
		t.Fatalf("播放尾巴里也应能打断：err=%v aborted=%v", err, sink.wasAborted())
	}
	if sent := brain.sentTypes(); len(sent) != 1 || sent[0] != "conversation.item.truncate" {
		t.Errorf("回答已结束：只截断不取消，发了 %v", sent)
	}
}

// 没被打断：等播完才返回。
func TestPlaysToTheEnd(t *testing.T) {
	brain, sink := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	a := newBargeInAssistant(brain, sink)
	brain.events <- chunk
	brain.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	returned := make(chan error, 1)
	go func() {
		_, err := a.playResponse(context.Background(), noSpan(), "item_1", time.Now())
		returned <- err
	}()
	select {
	case <-returned:
		t.Fatal("还没播完就返回了")
	case <-time.After(100 * time.Millisecond):
	}
	close(sink.release)
	if err := <-returned; err != nil {
		t.Errorf("正常播完应返回 nil，得到 %v", err)
	}
	if len(brain.sentTypes()) != 0 || sink.wasAborted() {
		t.Error("没被打断时不应发任何事件、不应停播")
	}
}

// unansweredBrain：事件由测试推送；收到取消也不确认（模拟服务端一直不回 response.done）。
type unansweredBrain struct {
	events chan realtimeproto.ServerEvent
}

func (unansweredBrain) Send(context.Context, realtimeproto.ClientEvent) error { return nil }
func (b unansweredBrain) Events() <-chan realtimeproto.ServerEvent            { return b.events }

// 被打断后服务端一直不确认取消：最多等 Timing.CancelConfirm 就不等了（不用默认的 3 秒）。
func TestCancelConfirmWaitComesFromTiming(t *testing.T) {
	brain, sink := unansweredBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	a := NewAssistant(Dependencies{Brain: brain, Speaker: sink}, Options{BargeIn: true,
		Timing: Timing{CancelConfirm: 30 * time.Millisecond}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	brain.events <- chunk
	brain.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗"}
	started := time.Now()
	if _, err := a.playResponse(context.Background(), noSpan(), "item_1", time.Now()); !errors.Is(err, errInterrupted) {
		t.Fatalf("应被打断：%v", err)
	}
	if waited := time.Since(started); waited >= DefaultTiming.CancelConfirm {
		t.Errorf("应只等 Timing.CancelConfirm（30ms），等了 %v", waited)
	}
}

// 全局的追踪只能换一次：这个包的测试共用一个能读出来的。
var (
	spanRecorderOnce sync.Once
	recordedSpans    = tracetest.NewSpanRecorder()
)

// endedSpansNamed 返回从调用这一刻起结束了的、名字是 name 的 span 有几个。
func endedSpansNamed(t *testing.T, name string) func() int {
	t.Helper()
	spanRecorderOnce.Do(func() { otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recordedSpans))) })
	before := len(recordedSpans.Ended())
	return func() int {
		count := 0
		for _, span := range recordedSpans.Ended()[before:] {
			if span.Name() == name {
				count++
			}
		}
		return count
	}
}

// 回答播到一半终端停下（关机、换麦克风、重连）：停播，播放的 span 也结束，不漏掉。
func TestPlaybackSpanEndsWhenStopped(t *testing.T) {
	ended := endedSpansNamed(t, "terminal.playback")
	brain, sink := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	a := newBargeInAssistant(brain, sink)
	ctx, cancel := context.WithCancel(context.Background())
	brain.events <- chunk
	returned := make(chan error, 1)
	go func() { _, err := a.playResponse(ctx, noSpan(), "item_1", time.Now()); returned <- err }()
	<-sink.opened
	cancel()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("停下时应返回 context.Canceled，得到 %v", err)
	}
	if !sink.wasAborted() {
		t.Error("停下时应停播")
	}
	if n := ended(); n != 1 {
		t.Errorf("停下时播放的 span 应结束一次，结束了 %d 次", n)
	}
}

// 播完、被打断：播放的 span 都只结束一次。
func TestPlaybackSpanEndsOnceWhenFinishedOrInterrupted(t *testing.T) {
	ended := endedSpansNamed(t, "terminal.playback")
	brain, sink := &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	close(sink.release)
	brain.events <- chunk
	brain.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	if _, err := newBargeInAssistant(brain, sink).playResponse(context.Background(), noSpan(), "item_1", time.Now()); err != nil {
		t.Fatal(err)
	}
	brain, sink = &scriptedBrain{events: make(chan realtimeproto.ServerEvent, 16)}, newSlowSink()
	brain.events <- chunk
	brain.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗"}
	if _, err := newBargeInAssistant(brain, sink).playResponse(context.Background(), noSpan(), "item_2", time.Now()); !errors.Is(err, errInterrupted) {
		t.Fatalf("应被打断：%v", err)
	}
	if n := ended(); n != 2 {
		t.Errorf("播完一次、被打断一次：播放的 span 应各结束一次，共结束了 %d 次", n)
	}
}
