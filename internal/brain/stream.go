package brain

import (
	"cmp"
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

// ErrStreamRejected：转写服务不接受识别流的设置（例如要 semantic_vad 而它没有说完判断模型）。
// StreamingRecognizer.OpenStream 返回的错误包着它（同时包着带错误码的 *realtimeproto.ProtocolError）时，
// 会话不再重开（同样的设置再连多少次都被拒，要改配置），把同一个错误码告诉终端一次。
var ErrStreamRejected = errors.New("识别流的设置被拒绝（原样再连没用，要改配置）")

// openStream 打开流式转写会话并开始转发它的事件。调用方持有 s.mu。
// 这次打不开（例如服务端先起来、haro-asr 还在加载模型、它卡住了没在期限内确认）时不报错：记下设置，在后台按断开后
// 重开的办法接着连，连上之前送来的音频丢掉；一直连不上时按重开的规则提示终端。
// 设置被拒（ErrStreamRejected）时返回这个错误：连接层把同一个错误码回给终端；会话不再去连。
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
	openCtx, cancel := context.WithTimeout(ctx, s.opts.StreamReopen.orDefault().OpenTimeout)
	defer cancel()
	stream, err := s.parts.Streaming.OpenStream(openCtx, config)
	switch {
	case err == nil:
		s.stream = stream
		go s.runStream(stream)
	case errors.Is(err, ErrStreamRejected):
		s.streamRefused(err, false)
		return err
	default:
		s.logger.Warn("打不开识别流：在后台接着连", "err", err)
		go s.runStream(nil)
	}
	return nil
}

// streamRefused 是识别流的设置被拒之后：不再重开（识别流一直是 nil，送来的音频照断开期间那样丢掉，不逐块回错误），
// 记一条错误日志，提示终端一次「听不见了」（lostNotified：这次断开已经提示过了就不再提示）。
// 错误码由调用方告诉终端：第一次打开时连接层把 openStream 返回的错误回给终端，重开时 reopenStream 自己发。
func (s *Session) streamRefused(err error, lostNotified bool) {
	s.logger.Error("识别服务不接受识别流的设置：不再重开，改好配置后让终端重连", "err", err)
	if !lostNotified {
		s.tryEmit(realtimeproto.HaroNotice{Kind: realtimeproto.NoticeTranscriptionLost})
	}
}

// runStream 转发识别流的事件；流意外结束（或一开始就没打开）时重开并接着转发，直到会话结束。
func (s *Session) runStream(stream RecognizerStream) {
	if stream == nil {
		stream = s.reopenStream()
	}
	for stream != nil {
		s.pumpStreamEvents(stream)
		select {
		case <-s.closed: // 会话结束时关掉的（Close 先关 closed 再关流）：不是断开，不重开
			return
		default:
		}
		s.logger.Warn("识别流断开了：开始重开")
		stream = s.reopenStream()
	}
}

// takeJob 取出（并去掉）等这句话识别结果的任务；没有时返回 nil。
func (s *Session) takeJob(item string) *transcriptionJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.pendingJobs[item]
	delete(s.pendingJobs, item)
	return job
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
	s.awake = false // 识别流处理到休眠之前报来的说话事件不转给终端、不回答（asleepLocked）
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.lifetime, wakeArbitrationTimeout)
	defer cancel()
	s.requestAwake(ctx, stream, false)
	return false
}

// requestAwake 请识别流醒 / 睡。醒 / 睡由识别流（haro-asr 的唤醒门）决定：它处理完回 haro.wake_word.state，
// 会话照回复改自己记的、转给终端（pumpStreamEvents）。
// 发不出去说明这条流坏了：记下要的状态，关掉它（转发事件的循环随之重开），重开后告诉新的流。
func (s *Session) requestAwake(ctx context.Context, stream RecognizerStream, awake bool) {
	err := stream.SetAwake(ctx, awake)
	if err == nil || ctx.Err() != nil { // ctx 结束：终端断开或会话结束，不用重开
		return
	}
	s.logger.Warn("切换唤醒状态没发给识别流：重开后再告诉它", "awake", awake, "err", err)
	s.mu.Lock()
	s.awake = awake
	s.mu.Unlock()
	_ = stream.Close()
}

// asleepLocked：会话用唤醒门、现在记着睡着。调用方持有 s.mu。
func (s *Session) asleepLocked() bool {
	return s.streamConfig != nil && s.streamConfig.WakeWord && !s.awake
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
			s.awake, s.lastWakeKeyword = true, e.Keyword // 识别流听到唤醒词时自己开了唤醒门
			s.mu.Unlock()
			s.emit(e)
		case realtimeproto.HaroWakeWordState: // 识别流处理完一次醒 / 睡的请求（终端的，或会话自己的）
			s.mu.Lock()
			s.awake = e.Awake
			if !e.Awake {
				s.speech = nil // 识别流睡着时丢掉说到一半的那句，不会再有「说完」
			}
			s.mu.Unlock()
			s.emit(e)
		case realtimeproto.InputAudioBufferSpeechStarted:
			// 睡着时识别流不报说话；记着睡着时收到的，是输掉唤醒仲裁后、识别流处理到休眠之前报的：不转给终端
			s.mu.Lock()
			asleep := s.asleepLocked()
			if !asleep {
				s.speech = &e
			}
			s.mu.Unlock()
			if asleep {
				s.logger.Info("睡着时收到开始说话：识别流还没处理到休眠，不理", "item", e.ItemID)
				continue
			}
			s.emit(e)
		case realtimeproto.InputAudioBufferSpeechStopped:
			s.mu.Lock()
			asleep := s.asleepLocked()
			s.mu.Unlock()
			if asleep {
				s.logger.Info("睡着时收到说完：识别流还没处理到休眠，不回答", "item", e.ItemID)
				continue
			}
			s.speechStopped(e)
		case realtimeproto.InputAudioTranscriptionCompleted:
			if job := s.takeJob(e.ItemID); job != nil {
				job.text = strings.TrimSpace(e.Transcript)
				job.speaker, job.speakerChecked, job.tentative = e.Speaker, e.SpeakerChecked, e.SpeakerTentative
				job.language, job.wake, job.bargeIn = e.Language, e.Wake, e.BargeIn
				job.audio = time.Duration(e.AudioMs) * time.Millisecond
				s.logger.Info("阶段耗时", "stage", "brain.recognize", "elapsed", since(job.started))
				close(job.done)
			}
		case realtimeproto.InputAudioTranscriptionFailed:
			if job := s.takeJob(e.ItemID); job != nil {
				job.err = realtimeproto.ProtocolErrorf(e.Code, "%s", e.Message)
				close(job.done)
			}
			s.emit(e)
		case realtimeproto.Error:
			s.emit(e)
		}
	}

	// 识别流结束。说到一半断的：这句不会再有「说完」，替它补一个，这一轮跟着下面按识别失败结束 ——
	// 终端照常收尾（不再算在说话、对话窗口照常倒计时），不用等主人下一次开口。会话结束时关掉的不算
	s.mu.Lock()
	speech := s.speech
	s.speech = nil
	s.mu.Unlock()
	select {
	case <-s.closed:
	default:
		if speech != nil {
			s.logger.Warn("说到一半识别流断开：这句丢了", "item", speech.ItemID)
			s.speechStopped(realtimeproto.InputAudioBufferSpeechStopped{ItemID: speech.ItemID, AudioEndMs: speech.AudioStartMs})
		}
	}
	// 让还在等识别结果的回答失败，而不是一直卡住
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

// speechStopped 是识别流判定说完：登记一个识别任务，告诉终端，并（默认）马上开始回答。
func (s *Session) speechStopped(e realtimeproto.InputAudioBufferSpeechStopped) {
	job := &transcriptionJob{item: e.ItemID, done: make(chan struct{}), started: time.Now()}
	s.mu.Lock()
	if s.speech != nil && s.speech.ItemID == e.ItemID {
		s.speech = nil
	}
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
}

// ReopenTiming 是打开识别流、意外断开后重开的节奏（Options.StreamReopen；零值 = DefaultReopenTiming）。
type ReopenTiming struct {
	Delay, MaxDelay time.Duration // 重试间隔：从 Delay 开始翻倍，最长 MaxDelay
	LostNoticeAfter time.Duration // 断开超过这么久还没重开，才提示终端一次（一闪而过的断开不打扰）
	// OpenTimeout：每次打开（第一次和重开）最多等这么久：连上、发完设置、等到转写服务确认。零值 = DefaultReopenTiming 的
	OpenTimeout time.Duration
}

// DefaultReopenTiming：haro-asr 在本机（同一个 Pod），重试间隔不用拉得很长；
// 提示的门槛和终端「服务端断开超过 10 秒才说一声」一致。
// 打开的期限是定值（haro-asr 在本机，正常几毫秒就确认）：第一次打开是在终端连接的读循环里等的，这期间读不到终端的
// 心跳回应，所以不能比心跳的超时（heartbeat.Default.Timeout）长。
var DefaultReopenTiming = ReopenTiming{Delay: 500 * time.Millisecond, MaxDelay: 5 * time.Second, LostNoticeAfter: 10 * time.Second,
	OpenTimeout: 3 * time.Second}

func (t ReopenTiming) orDefault() ReopenTiming {
	if t == (ReopenTiming{}) {
		return DefaultReopenTiming
	}
	t.OpenTimeout = cmp.Or(t.OpenTimeout, DefaultReopenTiming.OpenTimeout)
	return t
}

// streamRestoreTimeout：重开后把醒着还是睡着 / 正在播放告诉新的流，最多等这么久。
const streamRestoreTimeout = 3 * time.Second

// reopenStream 按打开时的设置重开识别流；重开后把会话里记着的「醒着还是睡着 / 正在播放」告诉新的流。
// 会话结束、或者设置被拒（ErrStreamRejected：不再重开，把同一个错误码告诉终端）时返回 nil。
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
		// 设置被拒：再连多少次都一样
		AbortOnErrors(ErrStreamRejected).
		OnRetryScheduled(func(event failsafe.ExecutionScheduledEvent[RecognizerStream]) { // 失败的当下就记（OnRetry 要等完间隔才调用）
			s.logger.Warn("重开识别流失败", "attempt", event.Attempts(), "retry_in", event.Delay.Round(time.Millisecond), "err", event.LastError())
			if !lostNotified && time.Since(lostAt) >= timing.LostNoticeAfter {
				lostNotified = true
				s.logger.Warn("识别流断开太久：提示终端现在听不见", "since", since(lostAt))
				s.emit(realtimeproto.HaroNotice{Kind: realtimeproto.NoticeTranscriptionLost})
			}
		}).Build()
	stream, err := failsafe.With(policy).WithContext(s.lifetime).Get(func() (RecognizerStream, error) {
		ctx, cancel := context.WithTimeout(s.lifetime, timing.OpenTimeout)
		defer cancel()
		return s.parts.Streaming.OpenStream(ctx, config)
	})
	if errors.Is(err, ErrStreamRejected) && s.lifetime.Err() == nil {
		s.emit(realtimeproto.ErrorEventFor("", err)) // 只这一次：之后不再重开，也就不会再报
		s.streamRefused(err, lostNotified)
		return nil
	}
	if err != nil { // 会话结束了（其余的错误一直重试）
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
	if config.WakeWord { // 醒着、睡着都告诉新的流：它的回复转给终端（断开期间终端要的切换这时才确认）
		if err := stream.SetAwake(ctx, awake); err != nil {
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
