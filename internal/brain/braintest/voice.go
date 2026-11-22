package braintest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Voice 是假的流式识别（brain.StreamingRecognizer）：会话打开识别流时交出一条 Stream，测试经 Stream 让「主人说话」。
type Voice struct {
	mu       sync.Mutex
	streams  []*Stream
	configs  []brain.StreamConfig
	fail     error
	attempts int // OpenStream 被调用的次数（含失败的）
}

// NewVoice 创建假的流式识别。
func NewVoice() *Voice { return &Voice{} }

// FailNextOpen 让之后的 OpenStream 返回 err（nil = 恢复正常）。
func (v *Voice) FailNextOpen(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fail = err
}

// OpenStream 交出一条新的 Stream。
func (v *Voice) OpenStream(_ context.Context, config brain.StreamConfig) (brain.RecognizerStream, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.attempts++
	if v.fail != nil {
		return nil, v.fail
	}
	stream := &Stream{events: make(chan realtimeproto.ServerEvent, 64)}
	v.streams, v.configs = append(v.streams, stream), append(v.configs, config)
	return stream, nil
}

// OpenAttempts 是会话一共试着打开过几次识别流（含失败的）：断开后第一次重开就说明会话已经发现断开了。
func (v *Voice) OpenAttempts() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.attempts
}

// Stream 是最近打开的一条识别流；还没打开时为 nil。
func (v *Voice) Stream() *Stream {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.streams) == 0 {
		return nil
	}
	return v.streams[len(v.streams)-1]
}

// Config 是最近一次打开识别流时的配置。
func (v *Voice) Config() brain.StreamConfig {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.configs) == 0 {
		return brain.StreamConfig{}
	}
	return v.configs[len(v.configs)-1]
}

// Opened 是一共打开过几条识别流。
func (v *Voice) Opened() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.streams)
}

// errStreamClosed 是识别流关闭后还想发事件时的错误。
var errStreamClosed = errors.New("braintest: 识别流已经关闭")

// Stream 是一条假的识别流（brain.RecognizerStream）：记下收到的音频和控制，事件由测试发出。
type Stream struct {
	events chan realtimeproto.ServerEvent

	mu        sync.Mutex
	closed    bool
	audio     [][]byte
	awake     []bool
	awakeFail error // 下一次 SetAwake 返回的错误（FailNextSetAwake）
	playback  []bool
	items     int
}

// SendAudio 记下音频。
func (s *Stream) SendAudio(_ context.Context, pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audio = append(s.audio, slices.Clone(pcm))
	return nil
}

// SetAwake 记下唤醒门的开关。不回复 haro.wake_word.state：识别服务的回复由测试用 Emit 发出。
func (s *Stream) SetAwake(_ context.Context, awake bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.awake = append(s.awake, awake)
	err := s.awakeFail
	s.awakeFail = nil
	return err
}

// FailNextSetAwake 让下一次 SetAwake 返回 err（像发不出去那样；照样记下这次调用）。
func (s *Stream) FailNextSetAwake(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.awakeFail = err
}

// SetPlayback 记下终端是否在播放。
func (s *Stream) SetPlayback(_ context.Context, playing bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.playback = append(s.playback, playing)
	return nil
}

// Events 是识别流发给会话的事件。
func (s *Stream) Events() <-chan realtimeproto.ServerEvent { return s.events }

// Close 关闭识别流（会话会把它当作连接断开）。
func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
	return nil
}

// Emit 像识别服务那样发出事件。识别流已关闭时返回错误；积压超过 64 个时阻塞。
func (s *Stream) Emit(events ...realtimeproto.ServerEvent) error {
	for _, event := range events {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errStreamClosed
		}
		select {
		case s.events <- event:
			s.mu.Unlock()
		default:
			s.mu.Unlock()
			return fmt.Errorf("braintest: 识别流积压了 %d 个事件，会话没有在读", cap(s.events))
		}
	}
	return nil
}

// NextItemID 是下一句话的编号（item_1、item_2……）。
func (s *Stream) NextItemID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items++
	return "item_" + strconv.Itoa(s.items)
}

// LastItemID 是最近一句话的编号；还没有时为空。
func (s *Stream) LastItemID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == 0 {
		return ""
	}
	return "item_" + strconv.Itoa(s.items)
}

// Audio 是收到的全部音频块。
func (s *Stream) Audio() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.audio)
}

// AwakeCalls 是收到的唤醒门开关（true = 唤醒）。
func (s *Stream) AwakeCalls() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.awake)
}

// PlaybackCalls 是收到的播放状态（true = 开始播放）。
func (s *Stream) PlaybackCalls() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.playback)
}
