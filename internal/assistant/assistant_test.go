package assistant_test

import (
	"bytes"
	"context"
	"io"
	"iter"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

// 终端 + 真实的 brain.Session + 替身硬件/模型：验证一轮对话从按键到播完的整条路径。

type instantTrigger struct{}

func (instantTrigger) WaitForStart(context.Context) error { return nil }
func (instantTrigger) WaitForStop(context.Context) error  { return nil }

type fakeRecorder struct{}

func (fakeRecorder) StartRecording(context.Context) (assistant.Recording, error) {
	return fakeRecording{}, nil
}

type fakeRecording struct{}

func (fakeRecording) Stop() (realtimeproto.PCMAudio, error) {
	// 1 秒的 16kHz 单声道静音，超过 MinSpeech
	return realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: make([]byte, 32000)}, nil
}

type fakeCamera struct{}

func (fakeCamera) LatestFrame(context.Context) ([]byte, time.Time, error) {
	return []byte("jpeg"), time.Now(), nil
}

type recordingSpeaker struct {
	mu      sync.Mutex
	opened  int
	audio   bytes.Buffer
	drained bool
}

func (s *recordingSpeaker) OpenStream(context.Context, realtimeproto.AudioFormat) (assistant.AudioStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened++
	return s, nil
}
func (s *recordingSpeaker) Write(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audio.Write(pcm)
	return nil
}
func (s *recordingSpeaker) Drain() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drained = true
	return nil
}
func (s *recordingSpeaker) snapshot() (opened int, audio string, drained bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened, s.audio.String(), s.drained
}
func (s *recordingSpeaker) Abort() error          { return nil }
func (s *recordingSpeaker) Played() time.Duration { return 0 }

type stubRecognizer struct{}

func (stubRecognizer) Recognize(context.Context, realtimeproto.PCMAudio) (string, error) {
	return "我旁边有什么", nil
}

type stubModel struct{}

func (stubModel) StreamReply(context.Context, []brain.ChatMessage, []brain.ToolSpec) iter.Seq2[brain.ReplyDelta, error] {
	return func(yield func(brain.ReplyDelta, error) bool) {
		for _, token := range []string{"你旁边有", "一个杯子。", "还有键盘。"} {
			if !yield(brain.ReplyDelta{Text: token}, nil) {
				return
			}
		}
	}
}
func (stubModel) Warmup(context.Context) error { return nil }

type echoSynthesizer struct{}

func (echoSynthesizer) Synthesize(_ context.Context, utterance brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		yield(realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1}, Data: []byte(utterance.Text)}, nil)
	}
}

func TestHandleTurnPlaysWholeAnswerInOneStream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	session := brain.NewSession(brain.Components{
		Recognizer: stubRecognizer{}, Model: stubModel{}, Synthesizer: echoSynthesizer{},
		NewSegmenter: func() brain.TextSegmenter { return speech.NewSentenceSegmenter(4, 40) },
	}, brain.Options{MaxHistoryTurn: 6}, logger)
	defer session.Close()
	speaker := &recordingSpeaker{}
	haro := assistant.NewAssistant(assistant.Components{
		Trigger: instantTrigger{}, Recorder: fakeRecorder{}, Camera: fakeCamera{}, Speaker: speaker, Brain: session,
	}, assistant.Options{MinSpeech: 700 * time.Millisecond}, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := haro.HandleTurn(ctx); err != nil {
		t.Fatal(err)
	}
	if speaker.opened != 1 {
		t.Errorf("播放流打开了 %d 次，应只开 1 次", speaker.opened)
	}
	if got, want := speaker.audio.String(), "你旁边有一个杯子。还有键盘。"; got != want {
		t.Errorf("播出内容 got %q, want %q", got, want)
	}
	if !speaker.drained {
		t.Error("回答结束后应等播放流播完")
	}
}
