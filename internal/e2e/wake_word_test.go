// Package e2e 放跨包的整条路径测试：终端（assistant）接同一进程里真实的服务端会话（brain.Session），
// 只替换硬件、语音识别、模型和语音合成。终端、服务端各自的测试用替身对端；两边怎么配合在这里测。
package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

// 唤醒词整轮：休眠 → 听到唤醒词 → 一口气说完的问题经模型、合成回到终端播出 → 对话窗口超时 → 识别休眠；
// 睡着时服务端的提示经会话到终端响一声。

func pcm(data string) realtimeproto.PCMAudio {
	return realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: []byte(data)}
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

// noKeyboard 是没有键盘的终端：一直等到取消。
type noKeyboard struct{}

func (noKeyboard) WaitForToggle(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type recordingSpeaker struct {
	mu    sync.Mutex
	audio bytes.Buffer
}

func (s *recordingSpeaker) OpenStream(context.Context, realtimeproto.AudioFormat) (assistant.AudioStream, error) {
	return s, nil
}
func (s *recordingSpeaker) Write(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audio.Write(pcm)
	return nil
}
func (s *recordingSpeaker) played() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audio.String()
}
func (s *recordingSpeaker) Drain() error          { return nil }
func (s *recordingSpeaker) Abort() error          { return nil }
func (s *recordingSpeaker) Played() time.Duration { return 0 }

// stubModel 每次都回同样的两句。
type stubModel struct{}

func (stubModel) StreamReply(context.Context, brain.ChatRequest) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, token := range []string{"你旁边有", "一个杯子。", "还有键盘。"} {
			if !yield(brain.ReplyDelta{Text: token}, nil) {
				return
			}
		}
	}
}
func (stubModel) Warmup(context.Context) error { return nil }

// echoSynthesizer 把句子的文字原样当成声音，播出了什么一看便知。
type echoSynthesizer struct{}

func (echoSynthesizer) Synthesize(_ context.Context, utterance brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		yield(realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1}, Data: []byte(utterance.Text)}, nil)
	}
}

// wakeStream 是替身转写会话：收到第 10 块音频时报告「唤醒 + 一句话」；记录 SetAwake，以及之后收到的全零音频块（半双工静音）。
// failSleeps 是前几次休眠要失败（模拟通知休眠失败）。
type wakeStream struct {
	mu          sync.Mutex
	chunks      int
	silentAfter int
	awakeCalls  []bool
	failSleeps  int
	events      chan realtimeproto.ServerEvent
	closeOnce   sync.Once
}

func (w *wakeStream) SendAudio(_ context.Context, pcm []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.chunks++
	if w.chunks > 10 && bytes.Count(pcm, []byte{0}) == len(pcm) {
		w.silentAfter++
	}
	if w.chunks == 10 {
		w.events <- realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗", AudioMs: 100}
		w.events <- realtimeproto.InputAudioBufferSpeechStarted{AudioStartMs: 100, ItemID: "item_1"}
		w.events <- realtimeproto.InputAudioBufferSpeechStopped{AudioEndMs: 200, ItemID: "item_1"}
		w.events <- realtimeproto.InputAudioTranscriptionCompleted{ItemID: "item_1", Transcript: "我旁边有什么"}
	}
	return nil
}
func (w *wakeStream) SetPlayback(context.Context, bool) error { return nil }
func (w *wakeStream) SetAwake(_ context.Context, awake bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.awakeCalls = append(w.awakeCalls, awake)
	if !awake && w.failSleeps > 0 {
		w.failSleeps--
		return errors.New("通知休眠失败")
	}
	return nil
}
func (w *wakeStream) Events() <-chan realtimeproto.ServerEvent { return w.events }
func (w *wakeStream) Close() error {
	w.closeOnce.Do(func() { close(w.events) })
	return nil
}
func (w *wakeStream) silentChunks() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.silentAfter
}
func (w *wakeStream) sleepCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.awakeCalls) - len(slices.DeleteFunc(slices.Clone(w.awakeCalls), func(awake bool) bool { return !awake }))
}

type wakeStreaming struct {
	stream *wakeStream
	mu     sync.Mutex
	config brain.StreamConfig
}

func (w *wakeStreaming) OpenStream(_ context.Context, config brain.StreamConfig) (brain.RecognizerStream, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.config = config
	return w.stream, nil
}
func (w *wakeStreaming) openedWith() brain.StreamConfig {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.config
}

func waitForPlayed(t *testing.T, speaker *recordingSpeaker, want string, deadline time.Time, what string) {
	t.Helper()
	for audio := speaker.played(); audio != want; audio = speaker.played() {
		if time.Now().After(deadline) {
			t.Fatalf("%s：播出 %q，应为 %q", what, audio, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWakeWordThroughServerSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 第一次让识别休眠失败：会话把失败交回终端，终端过一个对话窗口再试（不能停在「醒着且永不到期」）
	streaming := &wakeStreaming{stream: &wakeStream{events: make(chan realtimeproto.ServerEvent, 8), failSleeps: 1}}
	notices := brain.NewNoticeHub()
	session := brain.NewSession(brain.Components{
		Streaming: streaming, Front: brain.Agent{Model: stubModel{}}, Synthesizer: echoSynthesizer{}, Notices: notices,
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, logger)
	defer session.Close()

	speaker := &recordingSpeaker{}
	haro := assistant.NewAssistant(assistant.Components{
		Manual: noKeyboard{}, Microphone: loudMicrophone{}, Speaker: speaker, Brain: session,
	}, assistant.Options{TurnDetection: realtimeproto.TurnDetection{Type: "server_vad"}, EchoGuard: 10 * time.Millisecond,
		NoticeTone:  pcm("[叮咚]"),
		NoticeTones: map[string]realtimeproto.PCMAudio{realtimeproto.NoticeTranscriptionLost: pcm("[断开]")},
		WakeWord: assistant.WakeWordOptions{Phrase: "哈罗哈罗", IdleTimeout: 150 * time.Millisecond,
			SleepTone: pcm("[我去睡觉了]")}}, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- haro.Run(ctx) }()

	// 唤醒后的问题被回答；回答播完、对话窗口结束：识别休眠之后终端再说一声「我去睡觉了」
	deadline := time.Now().Add(3 * time.Second)
	want := "你旁边有一个杯子。还有键盘。[我去睡觉了]"
	waitForPlayed(t, speaker, want, deadline, "唤醒后的问题应被回答、休眠时说一声")
	if !streaming.openedWith().WakeWord {
		t.Error("终端要求唤醒门：服务端应以唤醒门模式打开转写会话")
	}
	if calls := streaming.stream.sleepCalls(); calls != 2 {
		t.Errorf("第一次让识别休眠失败后应再试一次：试了 %d 次", calls)
	}
	// 不能打断时（半双工）：回答期间终端上传的静音一直送到识别
	if streaming.stream.silentChunks() == 0 {
		t.Error("回答期间识别应收到静音（半双工），但一块静音都没收到")
	}

	// 睡着时服务端发来提示（后台调查查完了）：终端响一声；有专门提示音的种类（服务端听不见了）播那一声
	notices.Notify(brain.SharedPerson, realtimeproto.HaroNotice{Kind: realtimeproto.NoticeResearchDone})
	waitForPlayed(t, speaker, want+"[叮咚]", deadline, "睡着时服务端的提示应响一声")
	notices.Notify(brain.SharedPerson, realtimeproto.HaroNotice{Kind: realtimeproto.NoticeTranscriptionLost})
	waitForPlayed(t, speaker, want+"[叮咚][断开]", deadline, "服务端听不见了应播断开的提示音")

	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run 返回 %v，应为 context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后 Run 没有退出")
	}
}
