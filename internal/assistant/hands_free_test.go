package assistant_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"haro/internal/assistant"
	"haro/internal/brain"
	"haro/internal/realtimeproto"
	"haro/internal/speech"
)

// 免按键整轮：进入对话模式 → 麦克风持续上传 → 替身 VAD 判定说完 → 回答播完 → 退出。

type scriptedToggle struct {
	enters int
	leave  chan struct{}
}

func (s *scriptedToggle) WaitForEnter(ctx context.Context) error {
	s.enters++
	if s.enters > 1 {
		return io.EOF // 第二次：模拟终端输入结束，让 Run 退出
	}
	return nil
}
func (s *scriptedToggle) WaitForLeave(ctx context.Context) error {
	select {
	case <-s.leave:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// loudMicrophone 每 2ms 产出一块非静音的 20ms 音频。
type loudMicrophone struct{}

func (loudMicrophone) OpenStream(ctx context.Context) (assistant.MicrophoneStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream := &loudMicStream{cancel: cancel, chunks: make(chan realtimeproto.PCMAudio)}
	go func() {
		defer close(stream.chunks)
		for {
			chunk := realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1},
				Data: bytes.Repeat([]byte{0x40, 0x1F}, 320)}
			select {
			case stream.chunks <- chunk:
			case <-ctx.Done():
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return stream, nil
}

type loudMicStream struct {
	cancel context.CancelFunc
	chunks chan realtimeproto.PCMAudio
}

func (s *loudMicStream) Chunks() <-chan realtimeproto.PCMAudio { return s.chunks }
func (s *loudMicStream) Close() error                          { s.cancel(); return nil }

// vadStream 是替身转写会话：收到第 10 块音频时判定「说了一句话并说完」，之后记录是否收到静音块。
type vadStream struct {
	mu           sync.Mutex
	chunks       int
	silentAfter  int // 判定说完之后收到的全零音频块数（半双工静音）
	events       chan realtimeproto.ServerEvent
	closeOnce    sync.Once
	stoppedFired bool
}

func (v *vadStream) SendAudio(_ context.Context, pcm []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.chunks++
	if v.stoppedFired && bytes.Count(pcm, []byte{0}) == len(pcm) {
		v.silentAfter++
	}
	if v.chunks == 10 {
		v.stoppedFired = true
		v.events <- realtimeproto.InputAudioBufferSpeechStarted{AudioStartMs: 20, ItemID: "item_1"}
		v.events <- realtimeproto.InputAudioBufferSpeechStopped{AudioEndMs: 200, ItemID: "item_1"}
		v.events <- realtimeproto.InputAudioTranscriptionCompleted{ItemID: "item_1", Transcript: "我旁边有什么"}
	}
	return nil
}
func (v *vadStream) SetAwake(context.Context, bool) error     { return nil }
func (v *vadStream) SetPlayback(context.Context, bool) error  { return nil }
func (v *vadStream) Events() <-chan realtimeproto.ServerEvent { return v.events }
func (v *vadStream) Close() error {
	v.closeOnce.Do(func() { close(v.events) })
	return nil
}
func (v *vadStream) silentChunks() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.silentAfter
}

type vadStreaming struct{ stream *vadStream }

func (v vadStreaming) OpenStream(context.Context, brain.StreamConfig) (brain.RecognizerStream, error) {
	return v.stream, nil
}

func TestHandsFreeConversation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stream := &vadStream{events: make(chan realtimeproto.ServerEvent, 8)}
	session := brain.NewSession(brain.Components{
		Recognizer: stubRecognizer{}, Streaming: vadStreaming{stream: stream},
		Model: stubModel{}, Synthesizer: echoSynthesizer{},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, logger)
	defer session.Close()

	toggle := &scriptedToggle{leave: make(chan struct{})}
	speaker := &recordingSpeaker{}
	turn := realtimeproto.TurnDetection{Type: "server_vad", SilenceDurationMs: 700}
	haro := assistant.NewAssistant(assistant.Components{
		Toggle: toggle, Microphone: loudMicrophone{}, Camera: fakeCamera{}, Speaker: speaker, Brain: session,
	}, assistant.Options{TurnDetection: &turn, EchoGuard: 50 * time.Millisecond}, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- haro.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, _, drained := speaker.snapshot(); drained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等回答播完超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
	opened, audio, _ := speaker.snapshot()
	if opened != 1 || audio != "你旁边有一个杯子。还有键盘。" {
		t.Errorf("播放流打开 %d 次，播出 %q", opened, audio)
	}
	// 从请求回答到播完后的 EchoGuard（50ms）期间都应上传静音；替身回答是瞬间完成的，所以等一会儿再看
	for waitUntil := time.Now().Add(time.Second); stream.silentChunks() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(waitUntil) {
			t.Fatal("回答期间应上传静音（半双工），但一块静音都没收到")
		}
	}

	close(toggle.leave) // 按回车退出对话模式 → 再次等待进入时输入结束
	select {
	case err := <-runErr:
		if err != io.EOF {
			t.Errorf("Run 返回 %v，应为 io.EOF", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("退出对话模式后 Run 没有结束")
	}
}
