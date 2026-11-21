package assistant_test

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 终端测试共用的替身硬件（终端接真实的服务端会话的整条路径在 internal/e2e）。

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
