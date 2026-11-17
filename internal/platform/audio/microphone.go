package audio

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gen2brain/malgo"

	"haro/internal/assistant"
	"haro/internal/realtimeproto"
)

// recordFormat 是录音格式：16 kHz 单声道 pcm16（和 PipeWire 版一致；设备原生格式由 miniaudio 转换、重采样）。
var recordFormat = realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}

// micChunkBytes 是 20 ms 一块（Realtime / WebRTC 常用的粒度）。
const micChunkBytes = 16000 / 50 * 2

// micQueueChunks：送给上层的块最多排这么多（1 秒）；上层跟不上时丢最新的，不阻塞音频线程。
const micQueueChunks = 50

// Microphone 常驻录音（v2 免按键），实现 assistant.Microphone。
type Microphone struct {
	backend *Backend
	key     string // 空 = 系统默认（跟随切换）
}

// NewMiniaudioMicrophone 创建麦克风。key 来自 Backend.Microphones()；空 = 系统默认。
func NewMiniaudioMicrophone(backend *Backend, key string) *Microphone {
	return &Microphone{backend: backend, key: key}
}

// OpenStream 打开设备开始录音；ctx 取消或调用 Close 时结束；设备被拔掉时 Chunks 关闭。
func (m *Microphone) OpenStream(ctx context.Context) (assistant.MicrophoneStream, error) {
	id, err := m.backend.findDevice(malgo.Capture, m.key)
	if err != nil {
		return nil, err
	}
	stream := &micStream{backend: m.backend, chunks: make(chan realtimeproto.PCMAudio, micQueueChunks),
		done: make(chan struct{})}
	pointer, release := cDeviceID(id)
	stream.release = release
	config := malgo.DefaultDeviceConfig(malgo.Capture)
	config.Capture.DeviceID = pointer
	config.Capture.Format = malgo.FormatS16
	config.Capture.Channels = uint32(recordFormat.Channels)
	config.SampleRate = uint32(recordFormat.SampleRate)
	config.PeriodSizeInMilliseconds = 20
	config.Resampling.Linear.LpfOrder = 8 // 设备多是 48 kHz：降到 16 kHz 前多滤一些，少混叠
	stream.device, err = m.backend.openDevice(config, malgo.DeviceCallbacks{
		Data: func(_, input []byte, _ uint32) { stream.receive(input) },
		Stop: stream.deviceStopped,
	})
	if err != nil {
		release()
		return nil, fmt.Errorf("打开麦克风: %w", err)
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = stream.Close()
		case <-stream.done:
		}
	}()
	return stream, nil
}

type micStream struct {
	backend *Backend
	device  *malgo.Device
	release func()
	chunks  chan realtimeproto.PCMAudio
	done    chan struct{}

	mu        sync.Mutex
	assembler chunkAssembler
	closed    bool
	closeOnce sync.Once
}

// receive 在音频线程里被调用：攒成 20 ms 一块送出去（input 指向 C 内存，必须复制）。
func (s *micStream) receive(input []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for _, chunk := range s.assembler.push(input) {
		select {
		case s.chunks <- realtimeproto.PCMAudio{Format: recordFormat, Data: chunk}:
		default: // 上层跟不上：丢掉，绝不阻塞音频线程
		}
	}
}

// deviceStopped：设备停了（被拔掉，或我们自己关的）⇒ 关闭 Chunks，上层就知道麦克风没了。
func (s *micStream) deviceStopped() { s.finish() }

func (s *micStream) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.chunks)
	}
}

// Chunks 返回 20 ms 一块的音频；麦克风停止时关闭。
func (s *micStream) Chunks() <-chan realtimeproto.PCMAudio { return s.chunks }

// Close 停止录音并释放设备。
func (s *micStream) Close() error {
	s.closeOnce.Do(func() {
		s.finish()
		s.backend.closeDevice(s.device)
		s.release()
		close(s.done)
	})
	return nil
}

// chunkAssembler 把任意长度的 PCM 攒成固定 micChunkBytes 一块。
type chunkAssembler struct{ pending []byte }

func (a *chunkAssembler) push(pcm []byte) [][]byte {
	a.pending = append(a.pending, pcm...)
	var chunks [][]byte
	for len(a.pending) >= micChunkBytes {
		chunk := make([]byte, micChunkBytes)
		copy(chunk, a.pending)
		chunks = append(chunks, chunk)
		a.pending = a.pending[micChunkBytes:]
	}
	a.pending = append([]byte(nil), a.pending...) // 不让底层数组无限增长
	return chunks
}

// Recorder 按一次录一段（v1.5 按键模式），实现 assistant.AudioRecorder。
type Recorder struct{ microphone *Microphone }

// NewMiniaudioRecorder 创建录音器。key 同 NewMiniaudioMicrophone。
func NewMiniaudioRecorder(backend *Backend, key string) *Recorder {
	return &Recorder{microphone: NewMiniaudioMicrophone(backend, key)}
}

// StartRecording 开始录音，直到返回值的 Stop 被调用。等第一块数据到了再返回（用户的第一个字才不会被吃掉）。
func (r *Recorder) StartRecording(ctx context.Context) (assistant.Recording, error) {
	stream, err := r.microphone.OpenStream(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	recording := &recording{stream: stream, collected: make(chan []byte, 1), firstChunk: make(chan struct{})}
	go recording.collect()
	select {
	case <-recording.firstChunk:
	case <-time.After(2 * time.Second): // 拿不到数据也不卡死，Stop 时如实返回录到的内容
	case <-ctx.Done():
	}
	return recording, nil
}

type recording struct {
	stream     assistant.MicrophoneStream
	collected  chan []byte
	firstChunk chan struct{}
}

func (r *recording) collect() {
	var pcm []byte
	first := true
	for chunk := range r.stream.Chunks() {
		if first {
			close(r.firstChunk)
			first = false
		}
		pcm = append(pcm, chunk.Data...)
	}
	r.collected <- pcm
}

// Stop 结束录音，返回录到的 PCM。
func (r *recording) Stop() (realtimeproto.PCMAudio, error) {
	_ = r.stream.Close()
	return realtimeproto.PCMAudio{Format: recordFormat, Data: <-r.collected}, nil
}
