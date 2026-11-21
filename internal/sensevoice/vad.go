package sensevoice

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// vadResetAfter：VAD 连续收满这么多采样点（1 小时）后，在没人说话的空档重置一次。
// sherpa-onnx 1.x 的 VAD 内部用 32 位整数记采样点下标、只增不减，满 2^31 个（16kHz 下约 37 小时）就溢出、越界崩溃；
// Reset 会把下标清零。
const vadResetAfter = 60 * 60 * realtimeproto.TranscriptionSampleRate

// sileroVAD 是 SileroDetector 用到的 sherpa-onnx VAD 的方法（测试里换成假的）。
type sileroVAD interface {
	AcceptWaveform(samples []float32)
	IsSpeech() bool
	IsEmpty() bool
	Front() *sherpa.SpeechSegment
	Pop()
	Reset()
}

// SileroDetector 是 sherpa-onnx 的 Silero VAD，一个转写会话一个实例。
type SileroDetector struct {
	vad        sileroVAD
	release    func()
	logger     *slog.Logger
	resetAfter int       // 见 vadResetAfter
	replay     int       // 重置后把最近这么多个采样点重新喂回去（见 reset）
	recent     []float32 // 最近收到的采样点（至少 replay 个）
	fed        int       // 上次重置以来喂进库里的采样点数（库里的下标从 0 数到这里）
	offset     int       // 库里下标 0 在会话时间轴上的位置：库给的语音段位置加上它，才是会话里的位置
}

// replaySamples 是重置后重新喂回去的长度：盖住刚开口、库还没判定为说话的那一段（库在没检测到说话时
// 只留最近 2 个窗口加最短语音那么长的音频，判定开始说话时从那里往回算起点）；又要短于判定说完要的静音，
// 喂回去的音频里就不会有上一句的尾巴（不在说话时，上一句至少已经静了这么久）。
func replaySamples(silenceMs int) int {
	return max(0, min(realtimeproto.TranscriptionSampleRate/2, silenceMs*realtimeproto.TranscriptionSampleRate/1000-sileroWindow))
}

// sileroWindow 是 Silero 在 16kHz 下的固定窗口（采样点）。
const sileroWindow = 512

// NewSileroDetectorFactory 返回按会话 turn_detection 创建 VAD 的工厂。
func NewSileroDetectorFactory(modelPath string, logger *slog.Logger) (asrserver.VoiceDetectorFactory, error) {
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
				WindowSize:         sileroWindow,
				MaxSpeechDuration:  20, // 一口气说超过 20 秒就强制切段
			},
			SampleRate: realtimeproto.TranscriptionSampleRate,
			NumThreads: 1,
			Provider:   "cpu",
		}
		vad := sherpa.NewVoiceActivityDetector(&config, 60)
		if vad == nil {
			return nil, errors.New("sherpa-onnx 创建 VAD 失败")
		}
		return &SileroDetector{vad: vad, release: func() { sherpa.DeleteVoiceActivityDetector(vad) }, logger: logger,
			resetAfter: vadResetAfter, replay: replaySamples(silence)}, nil
	}, nil
}

// AcceptWaveform 喂入 16kHz 单声道采样。喂满 resetAfter 之后，等到不在说话、也没有待取的语音段时重置一次
// （见 vadResetAfter、reset）。
func (d *SileroDetector) AcceptWaveform(samples []float32) {
	if d.fed >= d.resetAfter && !d.vad.IsSpeech() && d.vad.IsEmpty() {
		d.reset()
	}
	d.vad.AcceptWaveform(samples)
	d.fed += len(samples)
	d.recent = append(d.recent, samples...)
	if len(d.recent) > 2*d.replay+len(samples) { // 不每次都挪：攒到两倍再只留最近 replay 个
		d.recent = append(d.recent[:0], d.recent[len(d.recent)-d.replay:]...)
	}
}

// reset 把库里的下标清零，再把最近 replay 个采样点重新喂回去：重置前刚开口的那一小段会被重新判定，
// 不会被切掉。之后库报的语音段位置从喂回去的第一个采样点数起，offset 换成它在会话里的位置。
func (d *SileroDetector) reset() {
	replay := d.recent[max(0, len(d.recent)-d.replay):]
	d.offset += d.fed - len(replay)
	d.vad.Reset()
	d.vad.AcceptWaveform(replay)
	d.fed = len(replay)
	d.logger.Info("VAD 已重置：库里的采样点下标清零", "session_minutes", d.offset/realtimeproto.TranscriptionSampleRate/60,
		"replayed_ms", len(replay)*1000/realtimeproto.TranscriptionSampleRate)
}

// IsSpeech 返回当前是否处在语音中。
func (d *SileroDetector) IsSpeech() bool { return d.vad.IsSpeech() }

// PopSegments 取出已经结束的语音段（位置是会话时间轴上的）。
func (d *SileroDetector) PopSegments() []asrserver.SpeechSegment {
	var segments []asrserver.SpeechSegment
	for !d.vad.IsEmpty() {
		segment := d.vad.Front()
		segments = append(segments, asrserver.SpeechSegment{StartSample: d.offset + segment.Start, SampleCount: len(segment.Samples)})
		d.vad.Pop()
	}
	return segments
}

// Close 释放原生资源。
func (d *SileroDetector) Close() { d.release() }
