package assistant_test

import (
	"bytes"
	"context"
	"iter"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 终端测试共用的替身硬件与模型（终端 + 真实的 brain.Session 的整条路径见 wake_word_test.go）。

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

type echoSynthesizer struct{}

func (echoSynthesizer) Synthesize(_ context.Context, utterance brain.Utterance) iter.Seq2[realtimeproto.PCMAudio, error] {
	return func(yield func(realtimeproto.PCMAudio, error) bool) {
		yield(realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1}, Data: []byte(utterance.Text)}, nil)
	}
}
