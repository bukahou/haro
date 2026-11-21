package asrserver

import (
	"math"
	"sync"

	"github.com/bukahou/haro/internal/voiceprint"
)

// SpeakerEmbedder 把一段 16 kHz 语音算成声纹向量（cgo 实现在 internal/sherpa）。
type SpeakerEmbedder interface {
	Embed(samples []float32) ([]float32, error)
}

// SpeakerCheck 是声纹比对（v3 H-3）：唤醒词要是登记过的人说的才醒；每句话标上是谁说的。
// 没有登记任何声纹时不比对（否则谁都用不了）。
type SpeakerCheck struct {
	Embedder    SpeakerEmbedder
	Voiceprints []voiceprint.Voiceprint // 登记了新声纹后整体替换（mu 保护）
	Threshold   float64                 // 相似度达到这个值才算认出来
	// WakeThreshold：唤醒词（只有 1 秒多，声纹算不稳）达到这个值就能唤醒（v3 BV）。
	// 0 或高于 Threshold 时按 Threshold。唤醒分数没到 Threshold 时，唤醒句不沿用唤醒时的声纹，照常比对。
	WakeThreshold float64
	// 远程登记（v4 C-3）：存到哪、用哪个模型算的；Dir 为空时不接受登记
	Dir   string
	Model string

	mu sync.RWMutex
}

func (c *SpeakerCheck) enabled() bool {
	return c != nil && c.Embedder != nil && len(c.voiceprints()) > 0
}

func (c *SpeakerCheck) voiceprints() []voiceprint.Voiceprint {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Voiceprints
}

// identify 返回说话人的名字（没认出来为空）和最高相似度。
func (c *SpeakerCheck) identify(samples []float32) (string, float64, error) {
	embedding, err := c.Embedder.Embed(samples)
	if err != nil {
		return "", 0, err
	}
	name, score := voiceprint.Match(c.voiceprints(), embedding, c.Threshold)
	return name, score, nil
}

// wakeThreshold 是唤醒用的门槛：没设或设得比 Threshold 还高时，就用 Threshold。
func (c *SpeakerCheck) wakeThreshold() float64 {
	if c.WakeThreshold <= 0 || c.WakeThreshold > c.Threshold {
		return c.Threshold
	}
	return c.WakeThreshold
}

// identifyWake 比对唤醒词的声纹：达到唤醒门槛就返回名字；sure 表示还达到了 Threshold（认得很准）。
func (c *SpeakerCheck) identifyWake(samples []float32) (name string, score float64, sure bool, err error) {
	embedding, err := c.Embedder.Embed(samples)
	if err != nil {
		return "", 0, false, err
	}
	name, score = voiceprint.Match(c.voiceprints(), embedding, c.wakeThreshold())
	return name, score, name != "" && score >= c.Threshold, nil
}

// wakeQuality 是一次唤醒的质量：声纹分数 + 音量（v4 D2 多终端仲裁用）。
type wakeQuality struct{ score, level float64 }

// rootMeanSquare 是一段声音的音量（RMS，0–1）。
func rootMeanSquare(samples []float32) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, sample := range samples {
		sum += float64(sample) * float64(sample)
	}
	return math.Sqrt(sum / float64(len(samples)))
}
