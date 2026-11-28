package brainclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// 带着设备凭证握手（Authorization: Bearer）；服务端不认（401 invalid_credential）时返回 ErrInvalidCredential（重连没用，要重新配对）。
func TestDialSendsCredentialAndReportsRejection(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(realtimeproto.NewHTTPError(http.StatusUnauthorized, realtimeproto.CodeInvalidCredential, "移除了"))
	}))
	defer server.Close()
	_, err := Dial(context.Background(), DialOptions{BaseURL: server.URL, Credential: "secret"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, realtimeproto.ErrInvalidCredential) {
		t.Errorf("被拒的凭证：%v", err)
	}
	if got != "Bearer secret" {
		t.Errorf("握手带的：%q", got)
	}
}
