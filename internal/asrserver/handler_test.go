package asrserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bukahou/haro/internal/realtimeproto"
)

type fakeTranscriber struct{}

func (fakeTranscriber) Transcribe(context.Context, realtimeproto.PCMAudio) (Transcript, error) {
	return Transcript{Text: "你好。", Language: "zh"}, nil
}

func TestHealth(t *testing.T) {
	handler := NewHandler(Dependencies{Transcriber: fakeTranscriber{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("状态码 %d", recorder.Code)
	}
}
