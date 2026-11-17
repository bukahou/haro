//go:build integration

// 用真实的 Smart Turn 模型给一批录音打分（默认不跑）：
//
//	HARO_TURN_MODEL=... HARO_ONNXRUNTIME_LIB=.../libonnxruntime.so HARO_TEST_TURN_WAVS=a.wav,b.wav \
//	  go test -tags integration -run DetectorLive -v ./internal/smartturn/
package smartturn

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetectorLive(t *testing.T) {
	detector, err := NewDetector(os.Getenv("HARO_TURN_MODEL"), os.Getenv("HARO_ONNXRUNTIME_LIB"))
	if err != nil {
		t.Fatal(err)
	}
	defer detector.Close()
	for _, path := range strings.Split(os.Getenv("HARO_TEST_TURN_WAVS"), ",") {
		wav, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pcm := wav[44:] // ffmpeg 生成的标准 44 字节头
		samples := make([]float32, len(pcm)/2)
		for i := range samples {
			samples[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		}
		started := time.Now()
		probability, err := detector.EndOfTurnProbability(samples)
		if err != nil {
			t.Fatal(err)
		}
		verdict := "没说完"
		if probability >= 0.5 {
			verdict = "说完了"
		}
		t.Logf("%-20s %.2fs  说完概率 %.3f → %s  （%s）", filepath.Base(path),
			float64(len(samples))/16000, probability, verdict, time.Since(started).Round(time.Millisecond))
	}
}
