package brainclient_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/brainclient"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// lockedBuffer 是给 slog 写、给测试读的缓冲区（服务端在另一个 goroutine 里写日志）。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForLog 等日志里出现 want（服务端写「终端已连接」时，终端那边可能已经握手完了）。
func waitForLog(t *testing.T, logs *lockedBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("日志里没有 %q：\n%s", want, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return logs.String()
}

func pinVersion(t *testing.T, version string) {
	saved := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = saved })
	buildinfo.Version = version
}

func newLoggedServer(logs *lockedBuffer) *httptest.Server {
	logger := slog.New(slog.NewTextHandler(logs, nil))
	return httptest.NewServer(brainserver.NewHandler(func() brainserver.Session { return newScriptedSession(false) }, brainserver.Options{}, logger))
}

// 服务端：握手响应里报自己的版本；终端的版本不同（或旧终端没报）时在日志里提醒。
func TestServerReportsVersionAndWarnsOnMismatch(t *testing.T) {
	pinVersion(t, "v0.5.0-aaaaaaa")
	for _, c := range []struct {
		name, terminal string
		warn           bool
	}{
		{"同版本", "v0.5.0-aaaaaaa", false},
		{"终端较旧", "v0.5.0-bbbbbbb", true},
		{"终端没报构建版本", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs lockedBuffer
			server := newLoggedServer(&logs)
			defer server.Close()
			header := http.Header{}
			if c.terminal != "" {
				header.Set(buildinfo.Header, c.terminal)
			}
			conn, response, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/realtime",
				&websocket.DialOptions{Subprotocols: []string{realtimeproto.ProtocolV2}, HTTPHeader: header})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if got := response.Header.Get(buildinfo.Header); got != "v0.5.0-aaaaaaa" {
				t.Errorf("握手响应里的服务端版本 = %q", got)
			}
			logged := waitForLog(t, &logs, "终端已连接")
			if warned := strings.Contains(logged, "level=WARN"); warned != c.warn {
				t.Errorf("提醒版本不同 = %v，应为 %v；日志：\n%s", warned, c.warn, logged)
			}
		})
	}
}

// 终端：握手时报自己的版本和 User-Agent；服务端的版本不同时在日志里提醒。
func TestClientReportsVersionAndWarnsOnMismatch(t *testing.T) {
	pinVersion(t, "v0.5.0-aaaaaaa")
	received := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.Header().Set(buildinfo.Header, "v0.5.1-ccccccc")
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{realtimeproto.ProtocolV2}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context()) // 一直等到终端断开
	}))
	defer server.Close()

	var logs lockedBuffer
	client, err := brainclient.Dial(context.Background(), brainclient.DialOptions{BaseURL: server.URL, InputFormat: mic16k}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	header := <-received
	if got := header.Get(buildinfo.Header); got != "v0.5.0-aaaaaaa" {
		t.Errorf("终端报的版本 = %q", got)
	}
	if got := header.Get("User-Agent"); got != "haro/v0.5.0-aaaaaaa" {
		t.Errorf("User-Agent = %q", got)
	}
	logged := logs.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "v0.5.1-ccccccc") {
		t.Errorf("应提醒服务端版本不同；日志：\n%s", logged)
	}
}
