package sensevoice

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/v2/sherpa_onnx"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// KeywordSpotterConfig 是唤醒词识别的配置。
type KeywordSpotterConfig struct {
	ModelDir     string  // sherpa-onnx KWS 模型目录（zipformer transducer：encoder / decoder / joiner + tokens.txt）
	KeywordsFile string  // 关键词文件：每行「拼音单元 … [:加分] [#门槛] @显示名」
	Score        float32 // 默认加分（关键词行里没写时用）
	Threshold    float32 // 默认触发门槛 0–1（越低越容易触发）
}

// NewKeywordSpotterFactory 加载唤醒词模型，返回「每个转写会话一个流」的工厂。模型在会话之间共享。
func NewKeywordSpotterFactory(config KeywordSpotterConfig) (asrserver.KeywordSpotterFactory, func(), error) {
	encoder, err := findModelFile(config.ModelDir, "encoder")
	if err != nil {
		return nil, nil, err
	}
	decoder, err := findModelFile(config.ModelDir, "decoder")
	if err != nil {
		return nil, nil, err
	}
	joiner, err := findModelFile(config.ModelDir, "joiner")
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(config.KeywordsFile); err != nil {
		return nil, nil, fmt.Errorf("关键词文件不可用: %w", err)
	}
	spotterConfig := sherpa.KeywordSpotterConfig{
		FeatConfig:        sherpa.FeatureConfig{SampleRate: realtimeproto.TranscriptionSampleRate, FeatureDim: 80},
		MaxActivePaths:    4,
		KeywordsFile:      config.KeywordsFile,
		KeywordsScore:     config.Score,
		KeywordsThreshold: config.Threshold,
	}
	spotterConfig.ModelConfig.Transducer = sherpa.OnlineTransducerModelConfig{Encoder: encoder, Decoder: decoder, Joiner: joiner}
	spotterConfig.ModelConfig.Tokens = filepath.Join(config.ModelDir, "tokens.txt")
	spotterConfig.ModelConfig.NumThreads = 1
	spotterConfig.ModelConfig.Provider = "cpu"
	spotter := sherpa.NewKeywordSpotter(&spotterConfig)
	if spotter == nil {
		return nil, nil, errors.New("sherpa-onnx 创建唤醒词识别器失败（模型或关键词文件有误？）")
	}
	factory := func() (asrserver.KeywordSpotter, error) {
		return &keywordStream{spotter: spotter, stream: sherpa.NewKeywordStream(spotter)}, nil
	}
	return factory, func() { sherpa.DeleteKeywordSpotter(spotter) }, nil
}

// findModelFile 在模型目录里找 encoder / decoder / joiner：优先 chunk-16，其次优先 int8 量化版。
func findModelFile(dir, part string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, part+"-*.onnx"))
	best, bestScore := "", -1
	for _, path := range matches {
		score := 0
		if strings.Contains(path, "chunk-16") {
			score += 2
		}
		if strings.HasSuffix(path, ".int8.onnx") {
			score++
		}
		if score > bestScore {
			best, bestScore = path, score
		}
	}
	if best == "" {
		return "", fmt.Errorf("在 %s 里找不到 %s-*.onnx", dir, part)
	}
	return best, nil
}

// keywordStream 是一个转写会话专用的唤醒词识别流。
type keywordStream struct {
	spotter *sherpa.KeywordSpotter
	stream  *sherpa.OnlineStream
}

// AcceptWaveform 喂入音频并返回识别到的唤醒词（没有则为空）。识别到后自动重置，避免同一次重复触发。
func (k *keywordStream) AcceptWaveform(samples []float32) string {
	k.stream.AcceptWaveform(realtimeproto.TranscriptionSampleRate, samples)
	for k.spotter.IsReady(k.stream) {
		k.spotter.Decode(k.stream)
		if keyword := k.spotter.GetResult(k.stream).Keyword; keyword != "" {
			k.spotter.Reset(k.stream)
			return keyword
		}
	}
	return ""
}

// Reset 丢掉已积累的状态（进入休眠时调用）。
func (k *keywordStream) Reset() { k.spotter.Reset(k.stream) }

// Close 释放流。
func (k *keywordStream) Close() { sherpa.DeleteOnlineStream(k.stream) }
