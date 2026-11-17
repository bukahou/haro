package sensevoice

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// SpeakerEmbedder 用说话人识别模型（sherpa-onnx，CAM++）把一段 16 kHz 语音算成声纹向量（v3 H-3）。
// 只算向量；比对和存取在纯 Go 的 voiceprint 包里。并发安全（同一时间只算一段）。
type SpeakerEmbedder struct {
	mu        sync.Mutex
	extractor *sherpa.SpeakerEmbeddingExtractor
	model     string
}

// NewSpeakerEmbedder 加载模型。
func NewSpeakerEmbedder(model string, threads int) (*SpeakerEmbedder, error) {
	if _, err := os.Stat(model); err != nil {
		return nil, fmt.Errorf("说话人识别模型不可用: %w", err)
	}
	extractor := sherpa.NewSpeakerEmbeddingExtractor(&sherpa.SpeakerEmbeddingExtractorConfig{
		Model: model, NumThreads: threads, Provider: "cpu"})
	if extractor == nil {
		return nil, errors.New("sherpa-onnx 创建说话人识别器失败（模型文件有误？）")
	}
	return &SpeakerEmbedder{extractor: extractor, model: model}, nil
}

// ModelName 是模型文件名（写进声纹文件：换了模型的旧声纹不能比）。
func (e *SpeakerEmbedder) ModelName() string { return filepath.Base(e.model) }

// Embed 算一段语音的声纹向量。太短（模型还没攒够特征）时报错。
func (e *SpeakerEmbedder) Embed(samples []float32) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stream := e.extractor.CreateStream()
	defer sherpa.DeleteOnlineStream(stream)
	stream.AcceptWaveform(realtimeproto.TranscriptionSampleRate, samples)
	stream.InputFinished()
	if !e.extractor.IsReady(stream) {
		return nil, errors.New("语音太短，算不出声纹")
	}
	return e.extractor.Compute(stream), nil
}

// Close 释放模型。
func (e *SpeakerEmbedder) Close() { sherpa.DeleteSpeakerEmbeddingExtractor(e.extractor) }
