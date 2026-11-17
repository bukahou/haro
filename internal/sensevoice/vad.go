package sensevoice

import (
	"errors"
	"fmt"
	"os"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"haro/internal/asrserver"
	"haro/internal/realtimeproto"
)

// SileroDetector 是 sherpa-onnx 的 Silero VAD，一个转写会话一个实例。
type SileroDetector struct {
	vad *sherpa.VoiceActivityDetector
}

// NewSileroDetectorFactory 返回按会话 turn_detection 创建 VAD 的工厂。
func NewSileroDetectorFactory(modelPath string) (asrserver.VoiceDetectorFactory, error) {
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("VAD 模型不可用: %w", err)
	}
	return func(turn realtimeproto.TurnDetection) (asrserver.VoiceDetector, error) {
		threshold := turn.Threshold
		if threshold <= 0 {
			threshold = 0.5
		}
		silence := turn.SilenceDurationMs
		if silence <= 0 {
			silence = 700
		}
		config := sherpa.VadModelConfig{
			SileroVad: sherpa.SileroVadModelConfig{
				Model:              modelPath,
				Threshold:          float32(threshold),
				MinSilenceDuration: float32(silence) / 1000,
				MinSpeechDuration:  0.25, // 短于 0.25 秒的声音（咳嗽、碰桌子）不算说话
				WindowSize:         512,  // Silero 在 16kHz 下的固定窗口
				MaxSpeechDuration:  20,   // 一口气说超过 20 秒就强制切段
			},
			SampleRate: realtimeproto.TranscriptionSampleRate,
			NumThreads: 1,
			Provider:   "cpu",
		}
		vad := sherpa.NewVoiceActivityDetector(&config, 60)
		if vad == nil {
			return nil, errors.New("sherpa-onnx 创建 VAD 失败")
		}
		return &SileroDetector{vad: vad}, nil
	}, nil
}

// AcceptWaveform 喂入 16kHz 单声道采样。
func (d *SileroDetector) AcceptWaveform(samples []float32) { d.vad.AcceptWaveform(samples) }

// IsSpeech 返回当前是否处在语音中。
func (d *SileroDetector) IsSpeech() bool { return d.vad.IsSpeech() }

// PopSegments 取出已经结束的语音段。
func (d *SileroDetector) PopSegments() []asrserver.SpeechSegment {
	var segments []asrserver.SpeechSegment
	for !d.vad.IsEmpty() {
		segment := d.vad.Front()
		segments = append(segments, asrserver.SpeechSegment{StartSample: segment.Start, SampleCount: len(segment.Samples)})
		d.vad.Pop()
	}
	return segments
}

// Close 释放原生资源。
func (d *SileroDetector) Close() { sherpa.DeleteVoiceActivityDetector(d.vad) }
