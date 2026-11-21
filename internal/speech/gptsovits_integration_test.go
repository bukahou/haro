//go:build integration

package speech

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/pcmwav"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// TestGPTSoVITSLive 连真实的 GPT-SoVITS 服务（haro.env 里的 HARO_TTS_*），按情绪各合成一句，测首段延迟。
// 设了 HARO_TEST_TTS_OUT 时把录音存进那个目录，供人耳试听。
//
//	source <你的 haro.env>
//	HARO_TEST_TTS_OUT=/tmp/x go test -tags integration -run TestGPTSoVITSLive -v ./internal/speech/
func TestGPTSoVITSLive(t *testing.T) {
	url, refDir, voiceFile := os.Getenv("HARO_TTS_URL"), os.Getenv("HARO_TTS_REF_DIR"), os.Getenv("HARO_TTS_VOICE_FILE")
	if url == "" || refDir == "" || voiceFile == "" {
		t.Skip("没有设 HARO_TTS_URL / HARO_TTS_REF_DIR / HARO_TTS_VOICE_FILE")
	}
	voice, err := LoadGPTSoVITSVoice(voiceFile)
	if err != nil {
		t.Fatal(err)
	}
	synth := NewGPTSoVITSSynthesizer(url, refDir, voice, 2, 30*time.Second, &http.Client{})
	out := os.Getenv("HARO_TEST_TTS_OUT")

	cases := []struct {
		name string
		u    brain.Utterance
	}{
		{"平常", brain.Utterance{Text: "现在是下午三点二十分，你四点有个会。"}},
		{"调侃", brain.Utterance{Text: "你问我冰箱里有什么？我又不是冰箱。", Emotion: brain.EmotionTease}},
		{"笑", brain.Utterance{Text: "哈哈，你又把袜子穿反了，今天第二次了。", Emotion: brain.EmotionLaugh}},
		{"激动", brain.Utterance{Text: "真的假的？明天放假？太好了！", Emotion: brain.EmotionExcited}},
		{"温柔", brain.Utterance{Text: "别着急，慢慢来，你已经做得很好了。", Emotion: brain.EmotionGentle}},
		{"轻声", brain.Utterance{Text: "嘘，已经很晚了，早点睡吧。", Emotion: brain.EmotionWhisper}},
		{"日语", brain.Utterance{Text: "えっ、また寝坊したの？しょうがないなあ、もう。", Emotion: brain.EmotionTease}},
	}
	for _, c := range cases {
		started := time.Now()
		var first time.Duration
		var audio realtimeproto.PCMAudio
		chunks := 0
		for chunk, err := range synth.Synthesize(context.Background(), c.u) {
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if chunks == 0 {
				first = time.Since(started)
				audio.Format = chunk.Format
			}
			chunks++
			audio.Data = append(audio.Data, chunk.Data...)
		}
		total := time.Since(started)
		t.Logf("%-4s 首段 %5s  总 %5s  音频 %5s  %d 段  %d Hz", c.name, first.Round(10*time.Millisecond),
			total.Round(10*time.Millisecond), audio.Duration().Round(10*time.Millisecond), chunks, audio.Format.SampleRate)
		if audio.Duration() < 500*time.Millisecond {
			t.Errorf("%s: 音频太短（%s）", c.name, audio.Duration())
		}
		if out != "" {
			if err := pcmwav.WriteFile(filepath.Join(out, c.name+".wav"), audio); err != nil {
				t.Fatal(err)
			}
		}
	}
}
