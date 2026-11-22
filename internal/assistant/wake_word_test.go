package assistant_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 唤醒词整轮（终端这一侧，对端是替身服务端）：休眠 → 听到唤醒词 → 一口气说完的问题被回答 → 对话窗口超时 → 通知休眠。
// 终端接真实的服务端会话的整条路径在 internal/e2e。

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

type blockingManual struct{ done chan struct{} }

func (b blockingManual) WaitForToggle(ctx context.Context) error {
	select {
	case <-b.done:
		return io.EOF // 模拟终端输入结束，让 Run 退出
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sleepFailure 是休眠请求怎么失败。
type sleepFailure int

const (
	sleepNotSent     sleepFailure = iota + 1 // 发不出去（Send 返回错误）
	sleepNotAnswered                         // 发出去了，语音前端一直不回复
)

// wakeServer 是替身服务端，只按协议收发事件：收到第 10 块音频时发「唤醒 + 一句话」和这一轮的回答（两句的声音）；
// 收到休眠请求时像语音前端那样回复睡着了。记下会话设置、休眠请求，以及之后收到的全零音频块（半双工静音）。
// failSleeps 是前几次休眠请求要失败，怎么失败由 failure 决定。
type wakeServer struct {
	mu          sync.Mutex
	update      realtimeproto.SessionUpdate
	chunks      int
	silentAfter int
	sleeps      int
	failSleeps  int
	failure     sleepFailure
	events      chan realtimeproto.ServerEvent
}

func (s *wakeServer) Send(_ context.Context, event realtimeproto.ClientEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e := event.(type) {
	case realtimeproto.SessionUpdate:
		s.update = e
	case realtimeproto.InputAudioBufferAppend:
		s.chunks++
		if s.chunks > 10 && bytes.Count(e.Audio.Data, []byte{0}) == len(e.Audio.Data) {
			s.silentAfter++
		}
		if s.chunks == 10 {
			for _, reply := range []realtimeproto.ServerEvent{
				realtimeproto.HaroWakeWordDetected{Keyword: "哈罗哈罗", AudioMs: 100},
				realtimeproto.InputAudioBufferSpeechStarted{AudioStartMs: 100, ItemID: "item_1"},
				realtimeproto.InputAudioBufferSpeechStopped{AudioEndMs: 200, ItemID: "item_1"},
				realtimeproto.InputAudioTranscriptionCompleted{ItemID: "item_1", Transcript: "我旁边有什么"},
				realtimeproto.ResponseOutputAudioDelta{Audio: pcm("你旁边有一个杯子。")},
				realtimeproto.ResponseOutputAudioDelta{Audio: pcm("还有键盘。")},
				realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted},
			} {
				s.events <- reply
			}
		}
	case realtimeproto.HaroWakeWordSleep:
		s.sleeps++
		if s.failSleeps > 0 {
			s.failSleeps--
			if s.failure == sleepNotSent {
				return errors.New("休眠请求发不出去")
			}
			return nil // 发出去了，但一直没有回复
		}
		s.events <- realtimeproto.HaroWakeWordState{Awake: false}
	}
	return nil
}
func (s *wakeServer) Events() <-chan realtimeproto.ServerEvent { return s.events }
func (s *wakeServer) snapshot() (update realtimeproto.SessionUpdate, silentChunks, sleeps int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update, s.silentAfter, s.sleeps
}

func waitForPlayed(t *testing.T, speaker *recordingSpeaker, want string, deadline time.Time, what string) {
	t.Helper()
	for _, audio, _ := speaker.snapshot(); audio != want; _, audio, _ = speaker.snapshot() {
		if time.Now().After(deadline) {
			t.Fatalf("%s：播出 %q，应为 %q", what, audio, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWakeWordConversation(t *testing.T) {
	// 第一次休眠没成（发不出去 / 语音前端没回复）：过一个对话窗口再试，不能停在「醒着且永不到期」；
	// 回复睡着了之后才说「我去睡觉了」
	for _, c := range []struct {
		name    string
		failure sleepFailure
	}{{"休眠请求发不出去", sleepNotSent}, {"语音前端没回复", sleepNotAnswered}} {
		t.Run(c.name, func(t *testing.T) { runWakeWordConversation(t, c.failure) })
	}
}

func runWakeWordConversation(t *testing.T, failure sleepFailure) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &wakeServer{events: make(chan realtimeproto.ServerEvent, 16), failSleeps: 1, failure: failure}
	manual := blockingManual{done: make(chan struct{})}
	speaker := &recordingSpeaker{}
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	haro := assistant.NewAssistant(assistant.Dependencies{
		Manual: manual, Microphone: loudMicrophone{}, Speaker: speaker, Brain: server,
	}, assistant.Options{TurnDetection: turn, EchoGuard: 10 * time.Millisecond,
		NoticeTone:  pcm("[叮咚]"),
		NoticeTones: map[string]realtimeproto.PCMAudio{realtimeproto.NoticeTranscriptionLost: pcm("[断开]")},
		WakeWord: assistant.WakeWordOptions{Phrase: "哈罗哈罗", IdleTimeout: 150 * time.Millisecond,
			SleepTone: pcm("[我去睡觉了]")},
		Timing: assistant.Timing{WakeConfirm: 50 * time.Millisecond}}, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- haro.Run(ctx) }()

	// 唤醒后一口气说完的问题被回答；回答播完、对话窗口结束：请求休眠，语音前端回复睡着了再说一声「我去睡觉了」
	deadline := time.Now().Add(3 * time.Second)
	want := "你旁边有一个杯子。还有键盘。[我去睡觉了]"
	waitForPlayed(t, speaker, want, deadline, "唤醒后的问题应被回答、休眠时说一声")

	update, silentChunks, sleeps := server.snapshot()
	if !update.WakeWord || update.TurnDetection == nil || *update.TurnDetection != turn {
		t.Errorf("开会话时应要求唤醒门、带上说完的判断方式：%+v", update)
	}
	if sleeps != 2 {
		t.Errorf("第一次休眠没成后应再试一次：请求了 %d 次", sleeps)
	}
	// 不能打断时（半双工）：从请求回答到播完后的 EchoGuard 期间都应上传静音
	if silentChunks == 0 {
		t.Error("回答期间应上传静音（半双工），但一块静音都没收到")
	}

	// 睡着时服务端发来提示（后台调查查完了）：响一声；有专门提示音的种类（服务端听不见了）播那一声
	server.events <- realtimeproto.HaroNotice{Kind: realtimeproto.NoticeResearchDone}
	waitForPlayed(t, speaker, want+"[叮咚]", deadline, "收到提示应响一声")
	server.events <- realtimeproto.HaroNotice{Kind: realtimeproto.NoticeTranscriptionLost}
	waitForPlayed(t, speaker, want+"[叮咚][断开]", deadline, "服务端听不见了应播断开的提示音")

	// 键盘输入结束（作为服务运行时 stdin 是空的）：不退出，照常靠唤醒词运行
	close(manual.done)
	select {
	case err := <-runErr:
		t.Fatalf("没有键盘不应退出：%v", err)
	case <-time.After(200 * time.Millisecond):
	}
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
