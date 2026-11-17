package brain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"haro/internal/realtimeproto"
)

// streamFormat 是免按键模式要求的音频格式（与转写会话一致）。
var streamFormat = realtimeproto.AudioFormat{SampleRate: realtimeproto.TranscriptionSampleRate, Channels: 1}

// openStream 打开流式转写会话并开始转发它的事件。调用方持有 s.mu。
func (s *Session) openStream(ctx context.Context, config StreamConfig) error {
	if s.parts.Streaming == nil {
		return errors.New("没有配置流式识别，无法使用免按键模式")
	}
	stream, err := s.parts.Streaming.OpenStream(ctx, config)
	if err != nil {
		return err
	}
	s.stream = stream
	s.pendingJobs = map[string]*transcriptionJob{}
	if config.WakeWord && s.parts.WakeArbiter != nil { // 参加多终端唤醒仲裁（v4 D2）
		s.parts.WakeArbiter.join(s)
	}
	go s.pumpStreamEvents(stream)
	return nil
}

// wakeArbitrationTimeout：输掉仲裁后让识别流回去休眠，最多等这么久。
const wakeArbitrationTimeout = 2 * time.Second

// winWake 在多台终端同时听到唤醒词时决定这台要不要回答（v4 D2）；输了就让这台的识别流回去休眠。
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
	ctx, cancel := context.WithTimeout(context.Background(), wakeArbitrationTimeout)
	defer cancel()
	if err := stream.SetAwake(ctx, false); err != nil {
		s.logger.Warn("让这台回去休眠失败", "err", err)
	}
	return false
}

// pumpStreamEvents 把转写会话的事件翻译给终端：
// speech_stopped 时登记一个识别任务（终端随后带上画面请求回答，回答会等这个任务完成），
// 识别结果到达时完成它。
func (s *Session) pumpStreamEvents(stream RecognizerStream) {
	for event := range stream.Events() {
		switch e := event.(type) {
		case realtimeproto.HaroWakeWordDetected:
			if !s.winWake(stream, e) {
				continue
			}
			s.mu.Lock()
			s.wakePending = true
			s.mu.Unlock()
			s.emit(e)
		case realtimeproto.InputAudioBufferSpeechStarted:
			s.emit(e)
		case realtimeproto.InputAudioBufferSpeechStopped:
			job := &transcriptionJob{done: make(chan struct{}), started: time.Now()}
			s.mu.Lock()
			s.pendingJobs[e.ItemID] = job
			s.transcription = job
			s.mu.Unlock()
			s.emit(e)
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

	// 连接断开：让还在等识别结果的回答失败，而不是一直卡住。
	s.mu.Lock()
	jobs := s.pendingJobs
	s.pendingJobs = map[string]*transcriptionJob{}
	s.stream = nil
	s.mu.Unlock()
	for _, job := range jobs {
		job.err = errors.New("转写服务连接断开")
		close(job.done)
	}
	select {
	case <-s.closed: // 正常关闭，不报错
	default:
		s.emit(realtimeproto.Error{Message: "转写服务连接断开，免按键模式不可用"})
	}
}
