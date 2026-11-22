package audio

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gen2brain/malgo"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 播放设备的缓冲：一个周期 20 ms、3 个周期 ⇒ 设备侧约 60 ms 延迟。
const (
	playbackPeriodMs = 20
	playbackPeriods  = 3
	// drainTail：队列放空之后，再等设备把自己缓冲里的声音放完（设备缓冲 + 余量）。
	drainTail = playbackPeriodMs*playbackPeriods*time.Millisecond + 80*time.Millisecond
	// drainPoll：等队列放空时多久看一次。
	drainPoll = 10 * time.Millisecond
	// drainSlack：等队列放空最多等「剩下的声音该播多久 + 这么久」（打开设备、蓝牙出声都要一点时间，约 0.2 s）。定值，
	// 没有单独实测：正常放完远用不了这么久；到点还没放空就当设备不再取数据（没报停也算），不让这一轮一直「在回答」
	drainSlack = 2 * time.Second
)

var (
	// errSpeakerGone：播放中设备停了（蓝牙断开、被拔掉）。
	errSpeakerGone = errors.New("播放设备断开了")
	// errSpeakerStalled：等播完时设备一直不取数据，也没报停。
	errSpeakerStalled = errors.New("播放设备不再取数据")
)

// Speaker 播放语音，实现 assistant.AudioSink。每轮回答打开一条流（打开时用当时的系统默认设备 ⇒ 跟随切换）。
type Speaker struct {
	backend *Backend
	key     string // 空 = 系统默认
}

// NewMiniaudioSpeaker 创建播放器。key 来自 Backend.Speakers()；空 = 系统默认。
func NewMiniaudioSpeaker(backend *Backend, key string) *Speaker {
	return &Speaker{backend: backend, key: key}
}

// OpenStream 打开设备，返回可以持续写入 PCM 的播放流。ctx 取消时立即停止。
func (p *Speaker) OpenStream(ctx context.Context, format realtimeproto.AudioFormat) (assistant.AudioStream, error) {
	id, err := p.backend.findDevice(malgo.Playback, p.key)
	if err != nil {
		return nil, err
	}
	stream := &playbackStream{backend: p.backend, format: format, done: make(chan struct{})}
	pointer, release := cDeviceID(id)
	stream.release = release
	config := malgo.DefaultDeviceConfig(malgo.Playback)
	config.Playback.DeviceID = pointer
	config.Playback.Format = malgo.FormatS16
	config.Playback.Channels = uint32(format.Channels)
	config.SampleRate = uint32(format.SampleRate)
	config.PeriodSizeInMilliseconds = playbackPeriodMs
	config.Periods = playbackPeriods
	stream.device, err = p.backend.openDevice(config, malgo.DeviceCallbacks{
		Data: func(output, _ []byte, _ uint32) { stream.queue.pull(output) },
		Stop: stream.queue.deviceStopped,
	})
	if err != nil {
		release()
		return nil, fmt.Errorf("打开音箱: %w", err)
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = stream.Abort()
		case <-stream.done:
		}
	}()
	return stream, nil
}

type playbackStream struct {
	backend *Backend
	device  *malgo.Device
	release func()
	format  realtimeproto.AudioFormat
	queue   pcmQueue
	done    chan struct{}
	once    sync.Once
}

// Write 把 PCM 排进队列（不阻塞；音频线程按播放速度取走）。
func (s *playbackStream) Write(pcm []byte) error {
	if !s.queue.push(pcm) {
		return errSpeakerGone
	}
	return nil
}

// Drain 等队列里的声音全部播完（最多等到该播的时长 + drainSlack），然后关闭设备。
func (s *playbackStream) Drain() error {
	err := s.queue.waitEmpty(s.format, drainSlack, drainPoll)
	if err == nil {
		time.Sleep(drainTail)
	}
	s.shutdown()
	return err
}

// Abort 立即停止并丢掉缓冲（打断用）。
func (s *playbackStream) Abort() error {
	s.queue.clear()
	s.shutdown()
	return nil
}

// Played 是已经交给设备的时长（按我们自己送出去的帧数算；设备侧再晚几十毫秒、蓝牙约 0.2 s 才真正出声）。
func (s *playbackStream) Played() time.Duration {
	return playDuration(s.queue.consumedBytes(), s.format)
}

// playDuration 是 bytes 字节的 PCM 按 format 播多久。
func playDuration(bytes int, format realtimeproto.AudioFormat) time.Duration {
	perSecond := format.BytesPerSecond()
	if perSecond == 0 {
		return 0
	}
	return time.Duration(bytes) * time.Second / time.Duration(perSecond)
}

func (s *playbackStream) shutdown() {
	s.once.Do(func() {
		s.backend.closeDevice(s.device)
		s.release()
		close(s.done)
	})
}

// pcmQueue 是写入方（播放器）和音频线程之间的缓冲。
type pcmQueue struct {
	mu       sync.Mutex
	pending  []byte
	consumed int
	stopped  bool // 设备停了（之后写入都失败）
}

// push 追加 PCM；设备已经停了返回 false。
func (q *pcmQueue) push(pcm []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return false
	}
	q.pending = append(q.pending, pcm...)
	return true
}

// pull 在音频线程里被调用：填满 output，不够的部分填静音（output 指向 C 内存）。
func (q *pcmQueue) pull(output []byte) {
	q.mu.Lock()
	n := copy(output, q.pending)
	q.pending = q.pending[n:]
	q.consumed += n
	q.mu.Unlock()
	clear(output[n:])
}

func (q *pcmQueue) deviceStopped() {
	q.mu.Lock()
	q.stopped = true
	q.mu.Unlock()
}

func (q *pcmQueue) clear() {
	q.mu.Lock()
	q.pending = nil
	q.mu.Unlock()
}

func (q *pcmQueue) status() (empty, stopped bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) == 0, q.stopped
}

// waitEmpty 等音频线程把队列取空：设备停了返回 errSpeakerGone；过了「现在剩下的声音该播多久 + slack」还没取空，
// 返回 errSpeakerStalled（设备不取数据、也没报停时，不一直等下去）。
func (q *pcmQueue) waitEmpty(format realtimeproto.AudioFormat, slack, poll time.Duration) error {
	q.mu.Lock()
	remaining := len(q.pending)
	q.mu.Unlock()
	deadline := time.Now().Add(playDuration(remaining, format) + slack)
	for {
		empty, stopped := q.status()
		switch {
		case stopped:
			return errSpeakerGone
		case empty:
			return nil
		case time.Now().After(deadline):
			return errSpeakerStalled
		}
		time.Sleep(poll)
	}
}

func (q *pcmQueue) consumedBytes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.consumed
}
