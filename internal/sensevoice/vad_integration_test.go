//go:build integration

// 用真实的 Silero VAD 验证中途重置（默认不跑）：
//
//	HARO_VAD_MODEL=... HARO_TEST_WAVS=a.wav go test -tags integration -run SileroResetLive -v ./internal/sensevoice/
package sensevoice

import (
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

// 同一段音频（静音、录音、静音、录音、静音）各跑一遍：不重置，和门槛调成 1 秒（静音里会重置好几次，
// 有的正好落在刚开口还没判定的时候）。切出来的语音段要一样多，每段的起点差不到一个窗口。
func TestSileroResetLive(t *testing.T) {
	const rate = realtimeproto.TranscriptionSampleRate
	path := strings.Split(os.Getenv("HARO_TEST_WAVS"), ",")[0]
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	audio, err := speech.DecodeWAV(data)
	if err != nil || audio.Format.SampleRate != rate || audio.Format.Channels != 1 {
		t.Fatalf("%s 必须是 16kHz 单声道：%+v %v", path, audio.Format, err)
	}
	recording := make([]float32, 0, len(audio.Data)/2)
	for i := 0; i+1 < len(audio.Data); i += 2 {
		recording = append(recording, float32(int16(uint16(audio.Data[i])|uint16(audio.Data[i+1])<<8))/32768)
	}
	var samples []float32
	for _, part := range [][]float32{make([]float32, 2*rate), recording, make([]float32, 2300*rate/1000), recording,
		make([]float32, 3*rate/2)} {
		samples = append(samples, part...)
	}

	factory, err := NewSileroDetectorFactory(os.Getenv("HARO_VAD_MODEL"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	run := func(resetAfter int) (segments []asrserver.SpeechSegment, offset int) {
		created, err := factory(realtimeproto.TurnDetection{})
		if err != nil {
			t.Fatal(err)
		}
		detector := created.(*SileroDetector)
		defer detector.Close()
		detector.resetAfter = resetAfter
		for i := 0; i < len(samples); i += 320 {
			detector.AcceptWaveform(samples[i:min(i+320, len(samples))])
			segments = append(segments, detector.PopSegments()...)
		}
		return segments, detector.offset
	}
	reference, _ := run(math.MaxInt)
	withResets, offset := run(rate)
	if offset == 0 {
		t.Fatal("门槛 1 秒：静音里应重置过")
	}
	if len(reference) == 0 || len(withResets) != len(reference) {
		t.Fatalf("重置不该改变切出几段：不重置 %d 段，重置 %d 段", len(reference), len(withResets))
	}
	for i := range reference {
		if diff := withResets[i].StartSample - reference[i].StartSample; diff < -sileroWindow || diff > sileroWindow {
			t.Errorf("第 %d 段起点：不重置 %d，重置 %d（差 %d 个采样点，超过一个窗口）",
				i+1, reference[i].StartSample, withResets[i].StartSample, diff)
		}
	}
	t.Logf("切出 %d 段；最后一次重置后库里下标 0 在会话第 %d 个采样点；起点（不重置 / 重置）：%v / %v",
		len(reference), offset, starts(reference), starts(withResets))
}

func starts(segments []asrserver.SpeechSegment) []int {
	var out []int
	for _, segment := range segments {
		out = append(out, segment.StartSample)
	}
	return out
}
