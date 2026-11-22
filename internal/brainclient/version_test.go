package brainclient_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/brainclient"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/realtimeproto"
)

func pinVersion(t *testing.T, version string) {
	saved := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = saved })
	buildinfo.Version = version
}

func newLoggedServer(logs *logRecords) *httptest.Server {
	return httptest.NewServer(brainserver.NewHandler(func(brainserver.Terminal) brainserver.Session { return newScriptedSession(false) },
		brainserver.Options{}, slog.New(logs)))
}

// 服务端：握手响应里报自己的版本；终端连上时记一条日志 —— 版本相同是 info（带终端的版本和协议），
// 终端的版本不同（或旧终端没报）是警告（带两边的版本），提醒升级。
func TestServerReportsVersionAndWarnsOnMismatch(t *testing.T) {
	const serverVersion = "v0.5.0-aaaaaaa"
	pinVersion(t, serverVersion)
	for _, c := range []struct {
		name, terminal string
		warn           bool
	}{
		{"同版本", serverVersion, false},
		{"终端较旧", "v0.5.0-bbbbbbb", true},
		{"终端没报构建版本", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := &logRecords{}
			server := newLoggedServer(logs)
			defer server.Close()
			header := http.Header{}
			if c.terminal != "" {
				header.Set(buildinfo.Header, c.terminal)
			}
			conn, response, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+realtimeproto.ConversationPath,
				&websocket.DialOptions{Subprotocols: []string{realtimeproto.Protocol}, HTTPHeader: header})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if got := response.Header.Get(buildinfo.Header); got != serverVersion {
				t.Errorf("握手响应里的服务端版本 = %q", got)
			}
			level, want := slog.LevelInfo, map[string]any{"version": buildinfo.Reported(c.terminal), "protocol": realtimeproto.Protocol}
			if c.warn {
				level, want = slog.LevelWarn, map[string]any{"terminal": buildinfo.Reported(c.terminal), "server": serverVersion}
			}
			logs.waitFor(t, "终端连上时的那条日志", func(r logRecord) bool { return r.level == level && r.hasFields(want) })
			if warnings := logs.matching(func(r logRecord) bool { return r.level == slog.LevelWarn }); !c.warn && len(warnings) > 0 {
				t.Errorf("版本相同不该提醒：%v", warnings)
			}
		})
	}
}

// 终端：握手时报自己的版本和 User-Agent；服务端的版本不同时在日志里提醒（警告，带两边的版本）。
func TestClientReportsVersionAndWarnsOnMismatch(t *testing.T) {
	pinVersion(t, "v0.5.0-aaaaaaa")
	received := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.Header().Set(buildinfo.Header, "v0.5.1-ccccccc")
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{realtimeproto.Protocol}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context()) // 一直等到终端断开
	}))
	defer server.Close()

	logs := &logRecords{}
	client, err := brainclient.Dial(context.Background(), brainclient.DialOptions{BaseURL: server.URL, InputFormat: mic16k}, slog.New(logs))
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
	versionWarning := func(r logRecord) bool {
		return r.level == slog.LevelWarn && r.hasFields(map[string]any{"terminal": "v0.5.0-aaaaaaa", "server": "v0.5.1-ccccccc"})
	}
	if found := logs.matching(versionWarning); len(found) != 1 { // Dial 返回之前就写了
		t.Errorf("应提醒一次服务端版本不同，提醒了 %d 次", len(found))
	}
}
