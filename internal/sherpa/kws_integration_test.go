//go:build integration

// 用真实的唤醒词模型逐个检测录音（默认不跑），按 20ms 一块喂入，模拟流式。模型、关键词文件、门槛按 haro-asr 的配置
// （同一套 HARO_* 环境变量，先在终端里载入；试别的门槛就改 HARO_KWS_THRESHOLD），再指定录音：
//
//	HARO_TEST_KWS_WAVS=a.wav,b.wav go test -tags integration -run KeywordSpotterLive -v ./internal/sherpa/
package sherpa

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/pcmwav"
)

// liveKeywordSpotterConfig 是按配置的唤醒词识别参数（和 cmd/haro-asr 的组装一致）。
func liveKeywordSpotterConfig(settings config.ASRSettings) KeywordSpotterConfig {
	return KeywordSpotterConfig{ModelDir: settings.KWSModelDir, EncoderFile: settings.KWSEncoderFile,
		DecoderFile: settings.KWSDecoderFile, JoinerFile: settings.KWSJoinerFile, KeywordsFile: settings.WakeWordsFile,
		Threshold: float32(settings.KWSThreshold), Score: float32(settings.KWSScore)}
}

func TestKeywordSpotterLive(t *testing.T) {
	spotterConfig := liveKeywordSpotterConfig(liveSettings(t))
	factory, release, err := NewKeywordSpotterFactory(spotterConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	t.Logf("门槛 %.2f", spotterConfig.Threshold)
	for _, path := range strings.Split(os.Getenv("HARO_TEST_KWS_WAVS"), ",") {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		audio, err := pcmwav.Decode(data)
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
