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

func TestLivezAndReadyz(t *testing.T) {
	handler := NewHandler(Dependencies{Transcriber: fakeTranscriber{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, path := range []string{"/livez", "/readyz"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s 状态码 %d", path, recorder.Code)
		}
	}
}
