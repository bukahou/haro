package sherpa

import (
	"errors"
	"fmt"
	"time"

	sherpaonnx "github.com/k2-fsa/sherpa-onnx-go/v2/sherpa_onnx"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// sileroWindowSize 是 Silero VAD 在 16 kHz 下的固定窗口（采样点，模型本身的要求）。
const sileroWindowSize = 512

// vadBufferPerMaxSpeech：VAD 内部的采样缓冲开成最长语音段的几倍。要装得下正在切的那一段（超过最长语音段之后
// 还会再长一点才切开）。定值：3 倍，最长语音段 20 s 时是 60 s
const vadBufferPerMaxSpeech = 3

// SileroDetector 是 sherpa-onnx 的 Silero VAD，一个转写会话一个实例。
type SileroDetector struct {
	vad *sherpaonnx.VoiceActivityDetector
}

// NewSileroDetectorFactory 返回按会话参数创建 VAD 的工厂（参数来自 asrserver.Timing 和会话的 turn_detection）。
func NewSileroDetectorFactory(modelPath string) (asrserver.VoiceDetectorFactory, error) {
	if err := requireFiles(modelPath); err != nil {
		return nil, fmt.Errorf("VAD 模型不可用: %w", err)
	}
	return func(detectorConfig asrserver.VoiceDetectorConfig) (asrserver.VoiceDetector, error) {
		config := sileroConfig(modelPath, detectorConfig)
		vad := sherpaonnx.NewVoiceActivityDetector(&config, float32(vadBuffer(detectorConfig).Seconds()))
		if vad == nil {
			return nil, errors.New("sherpa-onnx 创建 VAD 失败")
		}
		return &SileroDetector{vad: vad}, nil
	}, nil
}

// sileroConfig 把一个会话的 VAD 参数换成 sherpa-onnx 的配置。
func sileroConfig(modelPath string, config asrserver.VoiceDetectorConfig) sherpaonnx.VadModelConfig {
	return sherpaonnx.VadModelConfig{
		SileroVad: sherpaonnx.SileroVadModelConfig{
			Model:              modelPath,
			Threshold:          float32(config.Threshold),
			MinSilenceDuration: float32(config.MinSilence.Seconds()),
			MinSpeechDuration:  float32(config.MinSpeech.Seconds()),
			WindowSize:         sileroWindowSize,
			MaxSpeechDuration:  float32(config.MaxSpeech.Seconds()),
		},
		SampleRate: realtimeproto.TranscriptionSampleRate,
		NumThreads: 1,
		Provider:   "cpu",
	}
}

// vadBuffer 是 VAD 内部采样缓冲的大小。
func vadBuffer(config asrserver.VoiceDetectorConfig) time.Duration {
	return vadBufferPerMaxSpeech * config.MaxSpeech
}

// AcceptWaveform 喂入 16kHz 单声道采样。
func (d *SileroDetector) AcceptWaveform(samples []float32) { d.vad.AcceptWaveform(samples) }

// IsSpeech 返回当前是否处在语音中。
func (d *SileroDetector) IsSpeech() bool { return d.vad.IsSpeech() }

// PopSegments 取出已经结束的语音段（位置是会话时间轴上的：库里的起点从会话开始一直往上数，是 64 位的）。
func (d *SileroDetector) PopSegments() []asrserver.SpeechSegment {
	var segments []asrserver.SpeechSegment
	for !d.vad.IsEmpty() {
		segment := d.vad.Front()
		segments = append(segments, asrserver.SpeechSegment{StartSample: int(segment.Start), SampleCount: len(segment.Samples)})
		d.vad.Pop()
	}
	return segments
}

// Close 释放原生资源。
func (d *SileroDetector) Close() { sherpaonnx.DeleteVoiceActivityDetector(d.vad) }
