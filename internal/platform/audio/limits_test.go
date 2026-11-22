package audio

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// recordingHandler 收下警告级的日志记录（测试按记录的属性判断，不看日志文字）。
type recordingHandler struct {
	records chan slog.Record
}

func (h recordingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}
func (h recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.records <- record
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// chunksAttr 是一条日志里 chunks 属性的值；没有时 ok 为 false。
func chunksAttr(record slog.Record) (chunks int64, ok bool) {
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "chunks" {
			chunks, ok = attr.Value.Int64(), true
			return false
		}
		return true
	})
	return chunks, ok
}

// nextRecord 等下一条日志记录（日志在另一个 goroutine 里写）。
func nextRecord(t *testing.T, records chan slog.Record) slog.Record {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("等不到丢块的日志")
		return slog.Record{}
	}
}

// 上层跟不上时丢掉的块要计数：下一块送得出去时记一行丢了多少；关掉麦克风时还没记的也记一行。
func TestMicrophoneReportsDroppedChunks(t *testing.T) {
	records := make(chan slog.Record, 4)
	stream := &micStream{logger: slog.New(recordingHandler{records: records}), chunks: make(chan realtimeproto.PCMAudio, 1),
		done: make(chan struct{})}
	stream.receive(make([]byte, micChunkBytes*3)) // 队列只放得下 1 块：丢 2 块
	select {
	case record := <-records:
		t.Fatalf("还没送出下一块时不该记日志：%v", record.Message)
	case <-time.After(50 * time.Millisecond):
	}
	<-stream.chunks
	stream.receive(make([]byte, micChunkBytes)) // 送得出去了：记一行丢了 2 块
	if chunks, ok := chunksAttr(nextRecord(t, records)); !ok || chunks != 2 {
		t.Errorf("应记一行丢了 2 块：%d %v", chunks, ok)
	}
	stream.receive(make([]byte, micChunkBytes)) // 队列满（上一块没人取）：再丢 1 块
	stream.finish()
	if chunks, ok := chunksAttr(nextRecord(t, records)); !ok || chunks != 1 {
		t.Errorf("关掉麦克风时应记一行丢了 1 块：%d %v", chunks, ok)
	}
}

// 等播完有上限：设备一直不取数据（也没报停）时，过了「剩下的声音该播多久 + 余量」就返回 errSpeakerStalled，不一直等。
func TestDrainGivesUpWhenSpeakerStalls(t *testing.T) {
	format := realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}
	var queue pcmQueue
	queue.push(make([]byte, format.BytesPerSecond()/10)) // 100 ms 的声音，没人取
	started := time.Now()
	result := make(chan error, 1)
	go func() { result <- queue.waitEmpty(format, 50*time.Millisecond, time.Millisecond) }()
	select {
	case err := <-result:
		if !errors.Is(err, errSpeakerStalled) {
			t.Errorf("设备不取数据时应返回 errSpeakerStalled：%v", err)
		}
		if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
			t.Errorf("至少要等剩下的声音该播的时长（100 ms），只等了 %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("设备不取数据时等播完不该一直等下去")
	}
}

// 设备正常取数据就等到放空（nil）；设备停了返回 errSpeakerGone。
func TestDrainWaitsForPlaybackOrDeviceStop(t *testing.T) {
	format := realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}
	var queue pcmQueue
	queue.push(make([]byte, format.BytesPerSecond()/10))
	var device sync.WaitGroup
	device.Go(func() { // 假的音频线程：按 20 ms 一个周期取数据
		output := make([]byte, format.BytesPerSecond()/50)
		for {
			if empty, _ := queue.status(); empty {
				return
			}
			queue.pull(output)
			time.Sleep(20 * time.Millisecond)
		}
	})
	if err := queue.waitEmpty(format, time.Second, time.Millisecond); err != nil {
		t.Errorf("设备正常取数据时应等到放空：%v", err)
	}
	device.Wait()

	var stopped pcmQueue
	stopped.push([]byte{1, 2})
	stopped.deviceStopped()
	if err := stopped.waitEmpty(format, time.Second, time.Millisecond); !errors.Is(err, errSpeakerGone) {
		t.Errorf("设备停了应返回 errSpeakerGone：%v", err)
	}
}
