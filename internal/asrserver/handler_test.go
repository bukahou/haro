package asrserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/speech"
)

type fakeTranscriber struct{ got realtimeproto.PCMAudio }

func (f *fakeTranscriber) Transcribe(_ context.Context, audio realtimeproto.PCMAudio) (Transcript, error) {
	f.got = audio
	return Transcript{Text: "你好。", Language: "zh", Emotion: "HAPPY", Event: "Speech"}, nil
}

var testAudio = realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: make([]byte, 32000)}

// upload 构造 multipart 请求；file 为 nil 时不带文件字段。
func upload(t *testing.T, handler http.Handler, file []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if file != nil {
		part, _ := writer.CreateFormFile("file", "speech.wav")
		part.Write(file)
	}
	for key, value := range fields {
		writer.WriteField(key, value)
	}
	writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func newTestHandler() (http.Handler, *fakeTranscriber) {
	transcriber := &fakeTranscriber{}
	return NewHandler(Dependencies{Transcriber: transcriber, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}), transcriber
}

func TestTranscriptionJSON(t *testing.T) {
	handler, transcriber := newTestHandler()
	resp := upload(t, handler, speech.EncodeWAV(testAudio), map[string]string{"model": "whatever"})
	if resp.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", resp.Code, resp.Body)
	}
	var out map[string]string
	json.Unmarshal(resp.Body.Bytes(), &out)
	if out["text"] != "你好。" || len(out) != 1 {
		t.Errorf("响应 %v", out)
	}
	if transcriber.got.Format != testAudio.Format || len(transcriber.got.Data) != len(testAudio.Data) {
		t.Errorf("引擎收到的音频 %+v", transcriber.got.Format)
	}
}

func TestTranscriptionTextAndVerbose(t *testing.T) {
	handler, _ := newTestHandler()
	if resp := upload(t, handler, speech.EncodeWAV(testAudio), map[string]string{"response_format": "text"}); resp.Body.String() != "你好。" {
		t.Errorf("text 格式 %q", resp.Body)
	}
	resp := upload(t, handler, speech.EncodeWAV(testAudio), map[string]string{"response_format": "verbose_json"})
	var out map[string]any
	json.Unmarshal(resp.Body.Bytes(), &out)
	if out["language"] != "zh" || out["emotion"] != "HAPPY" || out["duration"] != 1.0 {
		t.Errorf("verbose_json %v", out)
	}
}

func TestTranscriptionBadRequests(t *testing.T) {
	handler, _ := newTestHandler()
	cases := map[string]*httptest.ResponseRecorder{
		"缺少文件":   upload(t, handler, nil, nil),
		"不是 WAV": upload(t, handler, []byte("garbage"), nil),
		"不支持的格式": upload(t, handler, speech.EncodeWAV(testAudio), map[string]string{"response_format": "srt"}),
	}
	for name, resp := range cases {
		if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "invalid_request_error") {
			t.Errorf("%s：状态码 %d，响应 %s", name, resp.Code, resp.Body)
		}
	}
}

func TestHealth(t *testing.T) {
	handler, _ := newTestHandler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("状态码 %d", recorder.Code)
	}
}
