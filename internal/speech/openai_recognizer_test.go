package speech

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"haro/internal/realtimeproto"
)

func TestOpenAITranscriptionRecognizer(t *testing.T) {
	audio := realtimeproto.PCMAudio{Format: realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}, Data: []byte{1, 0, 2, 0}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || r.Method != http.MethodPost {
			t.Errorf("请求 %s %s", r.Method, r.URL.Path)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("缺少 file 字段: %v", err)
		}
		wav, _ := io.ReadAll(file)
		decoded, err := DecodeWAV(wav)
		if err != nil || decoded.Format != audio.Format || string(decoded.Data) != string(audio.Data) {
			t.Errorf("上传的 WAV 不对: %+v, %v", decoded, err)
		}
		if r.FormValue("model") != "sensevoice-small" || r.FormValue("response_format") != "json" {
			t.Errorf("model=%q response_format=%q", r.FormValue("model"), r.FormValue("response_format"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"text": " 你好。 "})
	}))
	defer server.Close()

	recognizer := NewOpenAITranscriptionRecognizer(server.URL+"/", "sensevoice-small", server.Client())
	text, err := recognizer.Recognize(context.Background(), audio)
	if err != nil || text != "你好。" {
		t.Errorf("got %q, %v", text, err)
	}
}

func TestOpenAITranscriptionRecognizerHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"坏了"}}`, http.StatusInternalServerError)
	}))
	defer server.Close()
	recognizer := NewOpenAITranscriptionRecognizer(server.URL, "m", server.Client())
	if _, err := recognizer.Recognize(context.Background(), realtimeproto.PCMAudio{}); err == nil {
		t.Error("期望返回错误")
	}
}
