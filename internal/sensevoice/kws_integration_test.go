//go:build integration

// 用真实的唤醒词模型逐个检测录音（默认不跑），按 20ms 一块喂入，模拟流式：
//
//	HARO_KWS_MODEL_DIR=... HARO_KWS_KEYWORDS=... HARO_TEST_KWS_WAVS=a.wav,b.wav \
//	  go test -tags integration -run KeywordSpotterLive -v ./internal/sensevoice/
package sensevoice

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"haro/internal/speech"
)

func TestKeywordSpotterLive(t *testing.T) {
	threshold := float32(0.25)
	if value := os.Getenv("HARO_KWS_THRESHOLD"); value != "" {
		parsed, _ := strconv.ParseFloat(value, 32)
		threshold = float32(parsed)
	}
	factory, release, err := NewKeywordSpotterFactory(KeywordSpotterConfig{
		ModelDir: os.Getenv("HARO_KWS_MODEL_DIR"), KeywordsFile: os.Getenv("HARO_KWS_KEYWORDS"),
		Score: 1.0, Threshold: threshold,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	t.Logf("门槛 %.2f", threshold)
	for _, path := range strings.Split(os.Getenv("HARO_TEST_KWS_WAVS"), ",") {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		audio, err := speech.DecodeWAV(data)
		if err != nil {
			t.Fatal(err)
		}
		samples := pcmToFloat(audio.Data)
		spotter, _ := factory()
		var hits []string
		for offset := 0; offset < len(samples); offset += 320 {
			if keyword := spotter.AcceptWaveform(samples[offset:min(offset+320, len(samples))]); keyword != "" {
				hits = append(hits, keyword+"@"+strconv.Itoa(offset/16)+"ms")
			}
		}
		spotter.Close()
		t.Logf("%-14s 识别到：%v", filepath.Base(path), hits)
	}
}

func pcmToFloat(pcm []byte) []float32 {
	samples := make([]float32, len(pcm)/2)
	for i := range samples {
		samples[i] = float32(int16(uint16(pcm[2*i])|uint16(pcm[2*i+1])<<8)) / 32768
	}
	return samples
}
