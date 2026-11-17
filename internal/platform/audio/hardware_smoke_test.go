//go:build audiohw

// 真机冒烟测试（要真的麦克风和音箱）：go test -tags audiohw -v ./internal/platform/audio
// 只录 1 秒算音量（不保存），播 0.5 秒静音（听不到），验证打开 / 关闭 / 播完 / 已播时长。
package audio

import (
	"context"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

func TestHardwareSmoke(t *testing.T) {
	backend, err := NewMiniaudioBackend(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	microphones, _ := backend.Microphones()
	speakers, _ := backend.Speakers()
	for _, d := range microphones {
		t.Logf("🎤 %-70s default=%v %s", d.Key, d.Default, d.Name)
	}
	for _, d := range speakers {
		t.Logf("🔊 %-70s default=%v %s", d.Key, d.Default, d.Name)
	}

	ctx := context.Background()
	stream, err := NewMiniaudioMicrophone(backend, os.Getenv("SMOKE_MIC")).OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pcm []byte
	deadline := time.After(time.Second)
collect:
	for {
		select {
		case chunk := <-stream.Chunks():
			pcm = append(pcm, chunk.Data...)
		case <-deadline:
			break collect
		}
	}
	_ = stream.Close()
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(uint16(pcm[i])|uint16(pcm[i+1])<<8)) / 32768
		sum += v * v
	}
	t.Logf("录了 %d 字节（约 %.2f s），rms=%.4f", len(pcm), float64(len(pcm))/32000, math.Sqrt(sum/float64(max(1, len(pcm)/2))))
	if len(pcm) < 25000 {
		t.Errorf("1 秒应录到约 32000 字节")
	}

	format := realtimeproto.AudioFormat{SampleRate: 32000, Channels: 1}
	out, err := NewMiniaudioSpeaker(backend, "").OpenStream(ctx, format)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_ = out.Write(make([]byte, 32000)) // 0.5 s 静音
	if err := out.Drain(); err != nil {
		t.Fatal(err)
	}
	t.Logf("播 0.5 s 用了 %v，Played=%v", time.Since(started).Round(time.Millisecond), out.Played())
}
