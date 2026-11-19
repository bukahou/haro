package brain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// streamFormat 是识别流要求的音频格式（与转写会话一致）。
var streamFormat = realtimeproto.TranscriptionFormat

// openStream 打开流式转写会话并开始转发它的事件。调用方持有 s.mu。
// 这次打不开（例如服务端先起来、haro-asr 还在加载模型）时不报错：记下设置，在后台按断开后重开的办法接着连，
// 连上之前送来的音频丢掉；一直连不上时按重开的规则提示终端。
func (s *Session) openStream(ctx context.Context, config StreamConfig) error {
	if s.parts.Streaming == nil {
		return realtimeproto.ProtocolErrorf(realtimeproto.CodeNoTranscriptionStream, "服务端没有配置流式识别，打不开识别流")
	}
	s.streamConfig = &config
	s.awake = false // 新的流从休眠开始（启用唤醒词时；不用唤醒词时这个状态不起作用）
	s.pendingJobs = map[string]*transcriptionJob{}
	if config.WakeWord && s.parts.WakeArbiter != nil { // 参加多终端唤醒仲裁
		s.parts.WakeArbiter.join(s)
	}
	stream, err := s.parts.Streaming.OpenStream(ctx, config)
	if err != nil {
		s.logger.Warn("打不开识别流：在后台接着连", "err", err)
		go s.runStream(nil)
		return nil
	}
	s.stream = stream
	go s.runStream(stream)
	return nil
}

// runStream 转发识别流的事件；流意外结束（或一开始就没打开）时重开并接着转发，直到会话结束。
func (s *Session) runStream(stream RecognizerStream) {
	if stream == nil {
		stream = s.reopenStream()
	}
	for stream != nil {
		s.pumpStreamEvents(stream)
		s.logger.Warn("识别流断开了：开始重开")
		stream = s.reopenStream()
	}
}

// wakeArbitrationTimeout：输掉仲裁后让识别流回去休眠，最多等这么久。
const wakeArbitrationTimeout = 2 * time.Second

// winWake 在多台终端同时听到唤醒词时决定这台要不要回答；输了就让这台的识别流回去休眠。
func (s *Session) winWake(stream RecognizerStream, wake realtimeproto.HaroWakeWordDetected) bool {
	arbiter := s.parts.WakeArbiter
	if arbiter == nil {
		return true
	}
	s.mu.Lock()
	playing := s.playing
	s.mu.Unlock()
	if arbiter.claim(WakeClaim{Score: wake.Score, Level: wake.Level, Barge: playing, terminal: s}) {
		return true
	}
	s.logger.Info("几台终端同时听到唤醒词：另一台更合适，这台不回答",
		"score", fmt.Sprintf("%.2f", wake.Score), "level", fmt.Sprintf("%.4f", wake.Level))
	s.mu.Lock()
	s.awake = false
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), wakeArbitrationTimeout)
	defer cancel()
	if err := stream.SetAwake(ctx, false); err != nil {
		s.logger.Warn("让这台回去休眠失败", "err", err)
	}
	return false
}

// pumpStreamEvents 把转写会话的事件翻译给终端：
// speech_stopped 时登记一个识别任务，并（默认）马上开始回答——回答会等这个任务完成，识别和准备回答同时进行；
// 识别结果到达时完成它。
func (s *Session) pumpStreamEvents(stream RecognizerStream) {
	for event := range stream.Events() {
		switch e := event.(type) {
		case realtimeproto.HaroWakeWordDetected:
			if !s.winWake(stream, e) {
				continue
			}
			s.mu.Lock()
			s.wakePending, s.awake = true, true // 识别流听到唤醒词时自己开了唤醒门
			s.mu.Unlock()
			s.emit(e)
		case realtimeproto.InputAudioBufferSpeechStarted:
			s.emit(e)
		case realtimeproto.InputAudioBufferSpeechStopped:
			job := &transcriptionJob{item: e.ItemID, done: make(chan struct{}), started: time.Now()}
			s.mu.Lock()
			s.pendingJobs[e.ItemID] = job
			s.transcription = job
			s.mu.Unlock()
			s.emit(e)
			s.mu.Lock()
			var err error
			if s.autoRespond {
				err = s.startResponseLocked()
			}
			s.mu.Unlock()
			if err != nil {
				s.logger.Warn("判定说完时没能开始回答", "item", e.ItemID, "err", err)
				s.emit(realtimeproto.ErrorEventFor("", err))
			}
		case realtimeproto.InputAudioTranscriptionCompleted:
			s.mu.Lock()
			job := s.pendingJobs[e.ItemID]
			delete(s.pendingJobs, e.ItemID)
			s.mu.Unlock()
			if job != nil {
				job.text = strings.TrimSpace(e.Transcript)
				job.speaker, job.speakerChecked = e.Speaker, e.SpeakerChecked
				s.logger.Info("阶段耗时", "stage", "brain.recognize", "elapsed", since(job.started))
				close(job.done)
			}
		case realtimeproto.Error:
			s.emit(e)
		}
	}

	// 识别流结束：让还在等识别结果的回答失败，而不是一直卡住
	s.mu.Lock()
	jobs := s.pendingJobs
	s.pendingJobs = map[string]*transcriptionJob{}
	if s.stream == stream {
		s.stream = nil
	}
	s.mu.Unlock()
	for _, job := range jobs {
		job.err = errors.New("转写服务连接断开")
		close(job.done)
	}
	_ = stream.Close()
}

// ReopenTiming 是识别流意外断开后重开的节奏（Options.StreamReopen；零值 = DefaultReopenTiming）。
type ReopenTiming struct {
	Delay, MaxDelay time.Duration // 重试间隔：从 Delay 开始翻倍，最长 MaxDelay
	LostNoticeAfter time.Duration // 断开超过这么久还没重开，才提示终端一次（一闪而过的断开不打扰）
}

// DefaultReopenTiming：haro-asr 在本机（同一个 Pod），重试间隔不用拉得很长；
// 提示的门槛和终端「服务端断开超过 10 秒才说一声」一致。
var DefaultReopenTiming = ReopenTiming{Delay: 500 * time.Millisecond, MaxDelay: 5 * time.Second, LostNoticeAfter: 10 * time.Second}

func (t ReopenTiming) orDefault() ReopenTiming {
	if t == (ReopenTiming{}) {
		return DefaultReopenTiming
	}
	return t
}

const (
	streamOpenTimeout    = 5 * time.Second // 每次尝试重开最多等这么久（连上、发完设置）
	streamRestoreTimeout = 3 * time.Second // 重开后把醒着 / 正在播放告诉新的流，最多等这么久
)

// reopenStream 按打开时的设置重开识别流；重开后把会话里记着的「醒着 / 正在播放」告诉新的流。会话结束时返回 nil。
// 断开超过 LostNoticeAfter 时提示终端一次「听不见了」，重开后再提示一次「恢复了」。
func (s *Session) reopenStream() RecognizerStream {
	select {
	case <-s.closed: // 会话结束（正常关闭）
		return nil
	default:
	}
	s.mu.Lock()
	config := *s.streamConfig
	s.mu.Unlock()
	timing := s.opts.StreamReopen.orDefault()
	lostAt, lostNotified := time.Now(), false
	policy := retrypolicy.NewBuilder[RecognizerStream]().
		WithBackoff(timing.Delay, timing.MaxDelay).
		WithJitterFactor(0.1).
		WithMaxRetries(-1).
		OnRetryScheduled(func(event failsafe.ExecutionScheduledEvent[RecognizerStream]) { // 失败的当下就记（OnRetry 要等完间隔才调用）
			s.logger.Warn("重开识别流失败", "attempt", event.Attempts(), "retry_in", event.Delay.Round(time.Millisecond), "err", event.LastError())
			if !lostNotified && time.Since(lostAt) >= timing.LostNoticeAfter {
				lostNotified = true
				s.logger.Warn("识别流断开太久：提示终端现在听不见", "since", since(lostAt))
				s.emit(realtimeproto.HaroNotice{Kind: realtimeproto.NoticeTranscriptionLost})
			}
		}).Build()
	stream, err := failsafe.With(policy).WithContext(s.lifetime).Get(func() (RecognizerStream, error) {
		ctx, cancel := context.WithTimeout(s.lifetime, streamOpenTimeout)
		defer cancel()
		return s.parts.Streaming.OpenStream(ctx, config)
	})
	if err != nil { // 只有会话结束才会走到这里（一直重试）
		return nil
	}
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		_ = stream.Close()
		return nil
	default:
	}
	s.stream = stream
	awake, playing, dropped := s.awake, s.playing, s.droppedAudio
	s.droppedAudio = 0
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(s.lifetime, streamRestoreTimeout)
	if awake {
		if err := stream.SetAwake(ctx, true); err != nil {
			s.logger.Warn("重开后恢复唤醒状态失败", "err", err)
		}
	}
	if playing {
		if err := stream.SetPlayback(ctx, true); err != nil {
			s.logger.Warn("重开后恢复播放状态失败", "err", err)
		}
	}
	cancel()
	s.logger.Info("识别流已重开", "down_for", since(lostAt), "dropped_audio_chunks", dropped, "awake", awake, "playing", playing)
	if lostNotified {
		s.emit(realtimeproto.HaroNotice{Kind: realtimeproto.NoticeTranscriptionRestored})
	}
	return stream
}
