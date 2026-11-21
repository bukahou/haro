package sherpa

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	sherpaonnx "github.com/k2-fsa/sherpa-onnx-go/v2/sherpa_onnx"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Recognizer 封装 sherpa-onnx 的离线识别器（SenseVoice-Small）。
type Recognizer struct {
	// mu：识别器内部状态不保证并发安全，一次只识别一句；几台终端同时说完时在这里排队
	mu         sync.Mutex
	recognizer *sherpaonnx.OfflineRecognizer
}

// RecognizerConfig 是识别器的配置。模型文件由调用方给出（来自配置），不按文件名猜。
type RecognizerConfig struct {
	ModelDir  string // SenseVoice 模型目录（词表 tokens.txt 在这里）
	ModelFile string // 用目录里的哪个模型文件（例如 int8 量化版 model.int8.onnx）
	Threads   int    // 推理线程数
	Language  string // auto = 自动判断语种；也可以固定成 zh / en / ja / ko / yue
}

// NewRecognizer 加载模型。
func NewRecognizer(recognizerConfig RecognizerConfig) (*Recognizer, error) {
	modelPath := filepath.Join(recognizerConfig.ModelDir, recognizerConfig.ModelFile)
	tokensPath := filepath.Join(recognizerConfig.ModelDir, tokensFile)
	if err := requireFiles(modelPath, tokensPath); err != nil {
		return nil, fmt.Errorf("识别模型文件不可用: %w", err)
	}
	config := sherpaonnx.OfflineRecognizerConfig{}
	config.FeatConfig = sherpaonnx.FeatureConfig{SampleRate: 16000, FeatureDim: 80}
	config.ModelConfig.SenseVoice = sherpaonnx.OfflineSenseVoiceModelConfig{
		Model: modelPath, Language: recognizerConfig.Language, UseInverseTextNormalization: 1, // 数字写成阿拉伯数字、加标点
	}
	config.ModelConfig.Tokens = tokensPath
	config.ModelConfig.NumThreads = recognizerConfig.Threads
	config.ModelConfig.Provider = "cpu"
	recognizer := sherpaonnx.NewOfflineRecognizer(&config)
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
	stream := sherpaonnx.NewOfflineStream(r.recognizer)
	defer sherpaonnx.DeleteOfflineStream(stream)
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
	sherpaonnx.DeleteOfflineRecognizer(r.recognizer)
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
