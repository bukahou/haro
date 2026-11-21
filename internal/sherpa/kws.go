package sherpa

import (
	"errors"
	"fmt"
	"path/filepath"

	sherpaonnx "github.com/k2-fsa/sherpa-onnx-go/v2/sherpa_onnx"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// KeywordSpotterConfig 是唤醒词识别的配置。模型文件由调用方给出（来自配置），不按文件名猜。
type KeywordSpotterConfig struct {
	ModelDir string // sherpa-onnx 唤醒词模型目录（词表 tokens.txt 在这里）
	// 模型（zipformer transducer）用目录里的哪三个文件
	EncoderFile  string
	DecoderFile  string
	JoinerFile   string
	KeywordsFile string  // 关键词文件：每行「拼音单元 … [:加分] [#门槛] @显示名」
	Score        float32 // 默认加分（关键词行里没写时用）
	Threshold    float32 // 默认触发门槛 0–1（越低越容易触发）
}

// NewKeywordSpotterFactory 加载唤醒词模型，返回「每个转写会话一个流」的工厂。模型在会话之间共享。
func NewKeywordSpotterFactory(config KeywordSpotterConfig) (asrserver.KeywordSpotterFactory, func(), error) {
	transducer := sherpaonnx.OnlineTransducerModelConfig{Encoder: filepath.Join(config.ModelDir, config.EncoderFile),
		Decoder: filepath.Join(config.ModelDir, config.DecoderFile), Joiner: filepath.Join(config.ModelDir, config.JoinerFile)}
	tokens := filepath.Join(config.ModelDir, tokensFile)
	if err := requireFiles(transducer.Encoder, transducer.Decoder, transducer.Joiner, tokens); err != nil {
		return nil, nil, fmt.Errorf("唤醒词模型文件不可用: %w", err)
	}
	if err := requireFiles(config.KeywordsFile); err != nil {
		return nil, nil, fmt.Errorf("关键词文件不可用: %w", err)
	}
	spotterConfig := sherpaonnx.KeywordSpotterConfig{
		FeatConfig:        sherpaonnx.FeatureConfig{SampleRate: realtimeproto.TranscriptionSampleRate, FeatureDim: 80},
		MaxActivePaths:    4,
		KeywordsFile:      config.KeywordsFile,
		KeywordsScore:     config.Score,
		KeywordsThreshold: config.Threshold,
	}
	spotterConfig.ModelConfig.Transducer = transducer
	spotterConfig.ModelConfig.Tokens = tokens
	spotterConfig.ModelConfig.NumThreads = 1
	spotterConfig.ModelConfig.Provider = "cpu"
	spotter := sherpaonnx.NewKeywordSpotter(&spotterConfig)
	if spotter == nil {
		return nil, nil, errors.New("sherpa-onnx 创建唤醒词识别器失败（模型或关键词文件有误？）")
	}
	factory := func() (asrserver.KeywordSpotter, error) {
		return &keywordStream{spotter: spotter, stream: sherpaonnx.NewKeywordStream(spotter)}, nil
	}
	return factory, func() { sherpaonnx.DeleteKeywordSpotter(spotter) }, nil
}

// keywordStream 是一个转写会话专用的唤醒词识别流。
type keywordStream struct {
	spotter *sherpaonnx.KeywordSpotter
	stream  *sherpaonnx.OnlineStream
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
func (k *keywordStream) Close() { sherpaonnx.DeleteOnlineStream(k.stream) }
