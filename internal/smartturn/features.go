// Package smartturn 用 Smart Turn v3（Pipecat 开源，BSD-2-Clause）判断「这句话在语气上说完没有」。
//
// 本文件是纯 Go 的 Whisper 风格 log-mel 特征提取，逐步对照 Pipecat 的参考实现
// pipecat/audio/turn/smart_turn/_whisper_features.py（源自 HuggingFace transformers 的
// WhisperFeatureExtractor，Apache-2.0）：n_fft=400、hop=160、80 个 mel、Slaney 刻度与归一化、
// 周期 Hann 窗、反射填充、去掉最后一帧、log10 后截断到最大值 −8 再缩放。
package smartturn

import (
	"math"

	"gonum.org/v1/gonum/dsp/fourier"
)

const (
	sampleRate    = 16000
	windowSamples = sampleRate * 8 // 模型输入固定 8 秒
	nFFT          = 400
	hopLength     = 160
	nMels         = 80
	nFrames       = windowSamples / hopLength // 800
	melFloor      = 1e-10
	varianceEps   = 1e-7
)

// FeatureExtractor 计算 Smart Turn 的输入特征。预先算好窗函数与 mel 滤波器组；不可并发使用（FFT 有内部缓冲）。
type FeatureExtractor struct {
	window  []float64
	filters [][]float64 // [nMels][nFFT/2+1]
	fft     *fourier.FFT
}

// NewFeatureExtractor 创建特征提取器。
func NewFeatureExtractor() *FeatureExtractor {
	return &FeatureExtractor{window: periodicHann(nFFT), filters: melFilterbank(), fft: fourier.NewFFT(nFFT)}
}

// PrepareWindow 取最后 8 秒；不足 8 秒在**前面**补零（与 Pipecat 生产实现一致：语音贴在窗口末尾）。
func PrepareWindow(samples []float32) []float32 {
	if len(samples) >= windowSamples {
		return samples[len(samples)-windowSamples:]
	}
	padded := make([]float32, windowSamples)
	copy(padded[windowSamples-len(samples):], samples)
	return padded
}

// Compute 输入恰好 8 秒（128000 个采样点）的音频，返回按 [mel][frame] 展平的 80×800 特征。
func (e *FeatureExtractor) Compute(samples []float32) []float32 {
	// 1. 波形零均值、单位方差归一化（参考实现在 float32 下做）
	x := normalizeWaveform(samples)

	// 2. 反射填充 nFFT/2，逐帧加窗做实数 FFT，取功率谱
	pad := nFFT / 2
	padded := make([]float64, len(x)+2*pad)
	for i := range padded {
		padded[i] = float64(x[reflectIndex(i-pad, len(x))])
	}
	totalFrames := 1 + (len(padded)-nFFT)/hopLength // 801，最后一帧会丢掉
	bins := nFFT/2 + 1
	frame := make([]float64, nFFT)
	coeffs := make([]complex128, bins)
	logSpec := make([]float64, nMels*nFrames)
	maxLog := math.Inf(-1)
	power := make([]float64, bins)
	for f := 0; f < totalFrames-1; f++ {
		start := f * hopLength
		for i := range frame {
			frame[i] = padded[start+i] * e.window[i]
		}
		coeffs = e.fft.Coefficients(coeffs, frame)
		for k, c := range coeffs {
			power[k] = real(c)*real(c) + imag(c)*imag(c)
		}
		// 3. mel 投影 → log10
		for m := 0; m < nMels; m++ {
			sum := 0.0
			for k, weight := range e.filters[m] {
				sum += weight * power[k]
			}
			value := math.Log10(math.Max(sum, melFloor))
			logSpec[m*nFrames+f] = value
			maxLog = math.Max(maxLog, value)
		}
	}

	// 4. 截断到 (最大值 − 8)，再缩放到大约 [-1, 1]
	features := make([]float32, len(logSpec))
	for i, value := range logSpec {
		features[i] = float32((math.Max(value, maxLog-8) + 4) / 4)
	}
	return features
}

func normalizeWaveform(samples []float32) []float32 {
	var mean float64
	for _, s := range samples {
		mean += float64(s)
	}
	mean /= float64(len(samples))
	var variance float64
	for _, s := range samples {
		d := float64(s) - mean
		variance += d * d
	}
	variance /= float64(len(samples))
	scale := float32(1 / math.Sqrt(variance+varianceEps))
	normalized := make([]float32, len(samples))
	for i, s := range samples {
		normalized[i] = (s - float32(mean)) * scale
	}
	return normalized
}

// reflectIndex 实现 numpy 的 mode="reflect"（不重复边界点）。
func reflectIndex(i, n int) int {
	for i < 0 || i >= n {
		if i < 0 {
			i = -i
		}
		if i >= n {
			i = 2*(n-1) - i
		}
	}
	return i
}

// periodicHann 等同 np.hanning(n+1)[:-1]（即 torch.hann_window 的周期窗）。
func periodicHann(n int) []float64 {
	window := make([]float64, n)
	for i := range window {
		window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return window
}

func hertzToMelSlaney(freq float64) float64 {
	const minLogHertz, minLogMel = 1000.0, 15.0
	if freq >= minLogHertz {
		return minLogMel + math.Log(freq/minLogHertz)*(27.0/math.Log(6.4))
	}
	return 3.0 * freq / 200.0
}

func melToHertzSlaney(mel float64) float64 {
	const minLogHertz, minLogMel = 1000.0, 15.0
	if mel >= minLogMel {
		return minLogHertz * math.Exp(math.Log(6.4)/27.0*(mel-minLogMel))
	}
	return 200.0 * mel / 3.0
}

// melFilterbank 构造 Slaney 面积归一化的三角滤波器组（0–8000Hz）。
func melFilterbank() [][]float64 {
	bins := nFFT/2 + 1
	melMin, melMax := hertzToMelSlaney(0), hertzToMelSlaney(sampleRate/2)
	filterFreqs := make([]float64, nMels+2)
	for i := range filterFreqs {
		filterFreqs[i] = melToHertzSlaney(melMin + (melMax-melMin)*float64(i)/float64(nMels+1))
	}
	filters := make([][]float64, nMels)
	for m := range filters {
		filters[m] = make([]float64, bins)
		enorm := 2.0 / (filterFreqs[m+2] - filterFreqs[m])
		for k := 0; k < bins; k++ {
			fftFreq := float64(sampleRate/2) * float64(k) / float64(bins-1)
			down := (fftFreq - filterFreqs[m]) / (filterFreqs[m+1] - filterFreqs[m])
			up := (filterFreqs[m+2] - fftFreq) / (filterFreqs[m+2] - filterFreqs[m+1])
			filters[m][k] = math.Max(0, math.Min(down, up)) * enorm
		}
	}
	return filters
}
