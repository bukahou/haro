package smartturn

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

// testdata 由一份独立的 numpy 实现生成（numpy.fft，和这里的 gonum FFT 是两条路径）：
// input.pcm 是 Piper 合成的「你能看到我吗？做一下自我介绍。」（16kHz pcm16），features.f32 是 80×800 特征。
func TestComputeMatchesNumpyReference(t *testing.T) {
	pcm, err := os.ReadFile("testdata/input.pcm")
	if err != nil {
		t.Fatal(err)
	}
	reference, err := os.ReadFile("testdata/features.f32")
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]float32, len(pcm)/2)
	for i := range samples {
		samples[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	features := NewFeatureExtractor().Compute(PrepareWindow(samples))
	if len(features) != nMels*nFrames || len(reference) != 4*len(features) {
		t.Fatalf("长度 %d / 参考 %d", len(features), len(reference)/4)
	}
	maxDiff := 0.0
	for i, got := range features {
		want := math.Float32frombits(binary.LittleEndian.Uint32(reference[4*i:]))
		maxDiff = math.Max(maxDiff, math.Abs(float64(got-want)))
	}
	if maxDiff > 1e-3 {
		t.Errorf("与参考实现的最大误差 %.6f，超过 1e-3", maxDiff)
	}
	t.Logf("与参考实现的最大误差 %.2e", maxDiff)
}

func TestPrepareWindow(t *testing.T) {
	short := PrepareWindow([]float32{1, 2, 3})
	if len(short) != windowSamples || short[windowSamples-1] != 3 || short[0] != 0 {
		t.Error("不足 8 秒应在前面补零，语音贴在末尾")
	}
	long := make([]float32, windowSamples+5)
	long[len(long)-1] = 7
	if got := PrepareWindow(long); len(got) != windowSamples || got[windowSamples-1] != 7 {
		t.Error("超过 8 秒应保留最后 8 秒")
	}
}

func TestReflectIndex(t *testing.T) {
	// numpy reflect：[a b c d] 往左填充 2 个是 [c b | a b c d | c b]
	cases := map[int]int{-2: 2, -1: 1, 0: 0, 3: 3, 4: 2, 5: 1}
	for in, want := range cases {
		if got := reflectIndex(in, 4); got != want {
			t.Errorf("reflectIndex(%d) = %d，应为 %d", in, got, want)
		}
	}
}
