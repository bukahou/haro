// Package sensevoice 用 sherpa-onnx 在 CPU 上运行 SenseVoice-Small 做语音识别。
//
// ⚠️ 这是 haro 里唯一用 cgo 的包（sherpa-onnx 附带预编译的原生库），只允许 cmd/haro-asr 引用；
// haro 主程序经 HTTP 调用 haro-asr，保持纯 Go、可以直接交叉编译。
package sensevoice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Recognizer 封装 sherpa-onnx 的离线识别器。
type Recognizer struct {
	mu         sync.Mutex // 识别器内部状态不保证并发安全；haro 一次只说一句，串行即可
	recognizer *sherpa.OfflineRecognizer
}

// NewRecognizer 从模型目录（含 model.int8.onnx 与 tokens.txt）加载模型。language 为 "auto" 时自动判断语种。
func NewRecognizer(modelDir string, threads int, language string) (*Recognizer, error) {
	modelPath := filepath.Join(modelDir, "model.int8.onnx")
	tokensPath := filepath.Join(modelDir, "tokens.txt")
	for _, path := range []string{modelPath, tokensPath} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("模型文件不可用: %w", err)
		}
	}
	config := sherpa.OfflineRecognizerConfig{}
	config.FeatConfig = sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80}
	config.ModelConfig.SenseVoice = sherpa.OfflineSenseVoiceModelConfig{
		Model: modelPath, Language: language, UseInverseTextNormalization: 1, // 数字写成阿拉伯数字、加标点
	}
	config.ModelConfig.Tokens = tokensPath
	config.ModelConfig.NumThreads = threads
	config.ModelConfig.Provider = "cpu"
	recognizer := sherpa.NewOfflineRecognizer(&config)
	if recognizer == nil {
		return nil, errors.New("sherpa-onnx 创建识别器失败（模型文件是否完整？）")
	}
	return &Recognizer{recognizer: recognizer}, nil
}

// Transcribe 识别一段 16 位 PCM（采样率不限，sherpa-onnx 内部重采样；多声道只取第一声道）。
func (r *Recognizer) Transcribe(ctx context.Context, audio realtimeproto.PCMAudio) (asrserver.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return asrserver.Transcript{}, err
	}
	samples := pcm16ToFloat(audio)
	if len(samples) == 0 {
		return asrserver.Transcript{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stream := sherpa.NewOfflineStream(r.recognizer)
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(audio.Format.SampleRate, samples)
	r.recognizer.Decode(stream)
	result := stream.GetResult()
	return asrserver.Transcript{
		Text:     strings.TrimSpace(result.Text),
		Language: stripTag(result.Lang),
		Emotion:  stripTag(result.Emotion),
		Event:    stripTag(result.Event),
	}, nil
}

// Close 释放原生资源。
func (r *Recognizer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	sherpa.DeleteOfflineRecognizer(r.recognizer)
}

// pcm16ToFloat 把 s16le 转成 [-1, 1) 的 float32，多声道只取第一声道。
func pcm16ToFloat(audio realtimeproto.PCMAudio) []float32 {
	channels := max(audio.Format.Channels, 1)
	frameBytes := 2 * channels
	samples := make([]float32, 0, len(audio.Data)/frameBytes)
	for offset := 0; offset+frameBytes <= len(audio.Data); offset += frameBytes {
		value := int16(binary.LittleEndian.Uint16(audio.Data[offset:]))
		samples = append(samples, float32(value)/32768)
	}
	return samples
}

// stripTag 把 SenseVoice 的标记 "<|zh|>" 变成 "zh"。
func stripTag(tag string) string {
	return strings.TrimSuffix(strings.TrimPrefix(tag, "<|"), "|>")
}
