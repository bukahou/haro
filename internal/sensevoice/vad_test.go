package sensevoice

import (
	"io"
	"log/slog"
	"slices"
	"testing"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// fakeVAD 模拟 sherpa-onnx 的 VAD：下标从上次重置起数，语音段的位置也按这个下标报。
type fakeVAD struct {
	sinceReset []float32 // 上次重置以来收到的采样点
	speaking   bool
	pending    []*sherpa.SpeechSegment
	resets     int
}

func (f *fakeVAD) AcceptWaveform(samples []float32) { f.sinceReset = append(f.sinceReset, samples...) }
func (f *fakeVAD) IsSpeech() bool                   { return f.speaking }
func (f *fakeVAD) IsEmpty() bool                    { return len(f.pending) == 0 }
func (f *fakeVAD) Front() *sherpa.SpeechSegment     { return f.pending[0] }
func (f *fakeVAD) Pop()                             { f.pending = f.pending[1:] }
func (f *fakeVAD) Reset()                           { f.sinceReset, f.speaking, f.pending = nil, false, nil; f.resets++ }
func (f *fakeVAD) fed() int                         { return len(f.sinceReset) }
func (f *fakeVAD) finishSegment(start, samples int) {
	f.pending = append(f.pending, &sherpa.SpeechSegment{Start: start, Samples: make([]float32, samples)})
}

func newTestDetector(vad *fakeVAD, resetAfter, replay int) *SileroDetector {
	return &SileroDetector{vad: vad, release: func() {}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		resetAfter: resetAfter, replay: replay}
}

// chunkOf 是一块 320 个采样点、值都是 v 的音频（用来认出重置后喂回去的是哪一段）。
func chunkOf(v float32) []float32 {
	chunk := make([]float32, 320)
	for i := range chunk {
		chunk[i] = v
	}
	return chunk
}

// 喂满门槛之后，只在不在说话、没有待取的语音段时重置（重置不会切断一句话，也不会丢掉没取走的段）。
func TestSileroDetectorResetsOnlyBetweenSpeech(t *testing.T) {
	vad := &fakeVAD{}
	detector := newTestDetector(vad, 1000, 100)
	for range 4 { // 1280 个采样点：到门槛了，但重置发生在下一次喂之前
		detector.AcceptWaveform(chunkOf(0))
	}
	if vad.resets != 0 {
		t.Fatalf("还没到要重置的时候就重置了 %d 次", vad.resets)
	}

	vad.speaking = true
	detector.AcceptWaveform(chunkOf(0))
	if vad.resets != 0 {
		t.Fatal("正在说话时重置了：这句话会被切断")
	}

	vad.speaking = false
	vad.finishSegment(1000, 400)
	detector.AcceptWaveform(chunkOf(0))
	if vad.resets != 0 {
		t.Fatal("还有没取走的语音段时重置了：这段会丢")
	}

	if segments := detector.PopSegments(); len(segments) != 1 {
		t.Fatalf("应取出 1 段，实际 %d 段", len(segments))
	}
	detector.AcceptWaveform(chunkOf(0))
	if vad.resets != 1 {
		t.Fatalf("不说话、段也取走了：应重置一次，实际 %d 次", vad.resets)
	}
	if vad.fed() != 100+320 {
		t.Fatalf("重置后库里应只有喂回去的 100 个加新来的 320 个，实际 %d", vad.fed())
	}
}

// 重置后先把最近 replay 个采样点喂回去：重置前刚开口、库还没判定为说话的那一小段要重新判定，不能切掉。
func TestSileroDetectorReplaysRecentAudioAfterReset(t *testing.T) {
	vad := &fakeVAD{}
	detector := newTestDetector(vad, 1000, 400)
	for i := range 4 {
		detector.AcceptWaveform(chunkOf(float32(i + 1))) // 值 1..4：最后 400 个是 80 个 3 加 320 个 4
	}
	detector.AcceptWaveform(chunkOf(5)) // 喂之前重置
	if vad.resets != 1 {
		t.Fatalf("应重置一次，实际 %d 次", vad.resets)
	}
	want := slices.Concat(chunkOf(3)[:80], chunkOf(4), chunkOf(5))
	if !slices.Equal(vad.sinceReset, want) {
		t.Fatalf("重置后库里应是重置前最后 400 个采样点、再接新来的一块：got %d 个（开头 %v），want %d 个",
			len(vad.sinceReset), vad.sinceReset[:min(3, len(vad.sinceReset))], len(want))
	}
}

// 重置之后，库报的语音段位置从喂回去的第一个采样点数起；交给会话的位置要接着会话时间轴往下数
// （会话按它从录下的音频里取这一句）。
func TestSileroDetectorKeepsSessionTimelineAcrossReset(t *testing.T) {
	vad := &fakeVAD{}
	detector := newTestDetector(vad, 1000, 100)

	vad.finishSegment(50, 200)
	if segments := detector.PopSegments(); segments[0].StartSample != 50 {
		t.Fatalf("没重置过：位置应原样是 50，实际 %d", segments[0].StartSample)
	}

	session := 0
	for range 5 { // 前 4 次喂了 1280 个采样点；第 5 次喂之前重置
		detector.AcceptWaveform(chunkOf(0))
		session += 320
	}
	if vad.resets != 1 {
		t.Fatalf("应重置一次，实际 %d 次", vad.resets)
	}
	vad.finishSegment(vad.fed()+100, 200) // 库里：现在之后再过 100 个采样点开始说话
	segments := detector.PopSegments()
	want := session + 100
	if len(segments) != 1 || segments[0].StartSample != want || segments[0].SampleCount != 200 {
		t.Fatalf("重置后语音段的位置要接在会话时间轴上：want start=%d count=200，got %+v", want, segments)
	}
}

// 喂回去的长度：盖住刚开口还没判定的那一段（最多 0.5 秒），又短于判定说完要的静音。
func TestReplaySamples(t *testing.T) {
	for _, c := range []struct{ silenceMs, want int }{
		{700, 8000}, // 默认静音 0.7 秒：喂回 0.5 秒
		{300, 4288}, // 静音短：喂回的要比静音短一个窗口
		{20, 0},     // 比一个窗口还短：不喂回
	} {
		if got := replaySamples(c.silenceMs); got != c.want {
			t.Errorf("静音 %d 毫秒：want %d，got %d", c.silenceMs, c.want, got)
		}
	}
}
