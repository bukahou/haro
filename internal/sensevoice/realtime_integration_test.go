//go:build integration

// 用真实的 SenseVoice + Silero VAD 跑一遍流式转写会话（默认不跑）：
//
//	HARO_ASR_MODEL_DIR=... HARO_VAD_MODEL=... HARO_TEST_WAVS=a.wav,b.wav \
//	  go test -tags integration -run RealtimeLive -v ./internal/sensevoice/
package sensevoice

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/bukahou/haro/internal/asrserver"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

func TestRealtimeLive(t *testing.T) {
	recognizer, err := NewRecognizer(os.Getenv("HARO_ASR_MODEL_DIR"), 4, "auto")
	if err != nil {
		t.Fatal(err)
	}
	defer recognizer.Close()
	factory, err := NewSileroDetectorFactory(os.Getenv("HARO_VAD_MODEL"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(asrserver.NewHandler(asrserver.Dependencies{Transcriber: recognizer,
		NewVoiceDetector: factory, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer server.Close()

	// 拼接：0.5s 静音 + 录音 1 + 1.2s 静音 + 录音 2 + ... + 1.5s 静音
	silence := func(ms int) []byte { return make([]byte, ms*32) }
	stream := silence(500)
	var wavNames []string
	for _, path := range strings.Split(os.Getenv("HARO_TEST_WAVS"), ",") {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		audio, err := speech.DecodeWAV(data)
		if err != nil || audio.Format.SampleRate != 16000 || audio.Format.Channels != 1 {
			t.Fatalf("%s 必须是 16kHz 单声道：%+v %v", path, audio.Format, err)
		}
		stream = append(append(stream, audio.Data...), silence(1200)...)
		wavNames = append(wavNames, path[strings.LastIndex(path, "/")+1:])
	}
	stream = append(stream, silence(300)...)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+realtimeproto.TranscriptionPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	turn := realtimeproto.TurnDetection{Type: "server_vad", Threshold: 0.5, SilenceDurationMs: 700, PrefixPaddingMs: 300}
	if err := writeClientEvent(ctx, conn, transcriptionUpdate(turn, false)); err != nil {
		t.Fatal(err)
	}
	go func() {
		for offset := 0; offset < len(stream); offset += 640 {
			_ = writeClientEvent(ctx, conn, audioChunk(stream[offset:min(offset+640, len(stream))]))
		}
	}()

	completed := 0
	for completed < len(wavNames) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("已识别 %d 段，读取出错 %v", completed, err)
		}
		event, _ := realtimeproto.DecodeServerEvent(data)
		switch e := event.(type) {
		case realtimeproto.InputAudioBufferSpeechStarted:
			t.Logf("speech_started  %s  audio_start_ms=%d", e.ItemID, e.AudioStartMs)
		case realtimeproto.InputAudioBufferSpeechStopped:
			t.Logf("speech_stopped  %s  audio_end_ms=%d", e.ItemID, e.AudioEndMs)
		case realtimeproto.InputAudioTranscriptionCompleted:
			completed++
			t.Logf("completed       %s  %q", e.ItemID, e.Transcript)
		case realtimeproto.Error:
			t.Fatalf("error: %s", e.Message)
		}
	}
	t.Logf("输入 %d 段录音（%s），切出并识别 %d 段", len(wavNames), strings.Join(wavNames, ", "), completed)
}

// 唤醒门端到端（默认不跑）：HARO_KWS_MODEL_DIR / HARO_KWS_KEYWORDS 另需设置。
func TestWakeRealtimeLive(t *testing.T) {
	recognizer, err := NewRecognizer(os.Getenv("HARO_ASR_MODEL_DIR"), 4, "auto")
	if err != nil {
		t.Fatal(err)
	}
	defer recognizer.Close()
	vad, err := NewSileroDetectorFactory(os.Getenv("HARO_VAD_MODEL"))
	if err != nil {
		t.Fatal(err)
	}
	spotters, release, err := NewKeywordSpotterFactory(KeywordSpotterConfig{ModelDir: os.Getenv("HARO_KWS_MODEL_DIR"),
		KeywordsFile: os.Getenv("HARO_KWS_KEYWORDS"), Score: 1.0, Threshold: 0.25})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	server := httptest.NewServer(asrserver.NewHandler(asrserver.Dependencies{Transcriber: recognizer,
		NewVoiceDetector: vad, NewKeywordSpotter: spotters, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer server.Close()

	var stream []byte
	for _, path := range strings.Split(os.Getenv("HARO_TEST_WAVS"), ",") {
		data, _ := os.ReadFile(path)
		audio, err := speech.DecodeWAV(data)
		if err != nil {
			t.Fatal(err)
		}
		stream = append(append(stream, audio.Data...), make([]byte, 1500*32)...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+realtimeproto.TranscriptionPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	turn := realtimeproto.TurnDetection{Type: "server_vad", Threshold: 0.5, SilenceDurationMs: 700, PrefixPaddingMs: 300}
	_ = writeClientEvent(ctx, conn, transcriptionUpdate(turn, true))
	go func() {
		for offset := 0; offset < len(stream); offset += 640 {
			_ = writeClientEvent(ctx, conn, audioChunk(stream[offset:min(offset+640, len(stream))]))
		}
		_ = wsjson.Write(ctx, conn, map[string]string{"type": "haro.wake_word.sleep"}) // 流结束后的标记
	}()
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	for {
		_, data, err := conn.Read(readCtx)
		if err != nil {
			t.Logf("（读取结束：%v）", err)
			return
		}
		event, _ := realtimeproto.DecodeServerEvent(data)
		t.Logf("%s  %+v", event.Type(), event)
	}
}

// transcriptionUpdate 是转写会话的第一条消息。
func transcriptionUpdate(turn realtimeproto.TurnDetection, wakeWord bool) realtimeproto.SessionUpdate {
	return realtimeproto.SessionUpdate{Kind: realtimeproto.SessionTranscription, TurnDetection: &turn, WakeWord: wakeWord,
		InputFormat: realtimeproto.TranscriptionFormat}
}

// audioChunk 是一块 16 kHz 的音频。
func audioChunk(pcm []byte) realtimeproto.InputAudioBufferAppend {
	return realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: realtimeproto.TranscriptionFormat, Data: pcm}}
}

// writeClientEvent 把一个客户端事件编码后发出去。
func writeClientEvent(ctx context.Context, conn *websocket.Conn, event realtimeproto.ClientEvent) error {
	data, err := realtimeproto.EncodeClientEvent(event)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
