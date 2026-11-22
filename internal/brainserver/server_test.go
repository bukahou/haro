package brainserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/realtime"

	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/heartbeat"
	"github.com/bukahou/haro/internal/realtimeproto"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeSession 记下收到的事件；回答请求一律答「已有进行中的回答」（测错误码原样传给客户端）；
// 声明的输入格式不是 16 kHz 的 session.update 按 unsupported_audio_format 拒绝（和 brain.Session 一样）。
type fakeSession struct {
	received  chan realtimeproto.ClientEvent
	events    chan realtimeproto.ServerEvent
	closed    chan struct{}
	closeOnce sync.Once
}

func newFakeSession() *fakeSession {
	return &fakeSession{received: make(chan realtimeproto.ClientEvent, 16), events: make(chan realtimeproto.ServerEvent, 16),
		closed: make(chan struct{})}
}

func (s *fakeSession) Send(_ context.Context, event realtimeproto.ClientEvent) error {
	s.received <- event
	switch e := event.(type) {
	case realtimeproto.ResponseCreate:
		return realtimeproto.ProtocolErrorf(realtimeproto.CodeResponseInProgress, "已有进行中的回答")
	case realtimeproto.SessionUpdate:
		if e.InputFormat.SampleRate > 0 && e.InputFormat != realtimeproto.TranscriptionFormat {
			return realtimeproto.ProtocolErrorf(realtimeproto.CodeUnsupportedAudioFormat, "只收 16 kHz")
		}
	}
	return nil
}

func (s *fakeSession) Events() <-chan realtimeproto.ServerEvent { return s.events }

func (s *fakeSession) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func newServer(t *testing.T, session *fakeSession, opts brainserver.Options) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(brainserver.NewHandler(func(brainserver.Terminal) brainserver.Session { return session }, opts, quiet))
	t.Cleanup(server.Close)
	return server
}

func wsURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http") + realtimeproto.ConversationPath
}

// 握手时按子协议协商：haro 现在的版本接受；别的 haro 版本、带 haro 版本头却没报子协议（旧终端）拒绝；什么都没报当普通客户端。
func TestHandshakeNegotiatesProtocolVersion(t *testing.T) {
	for _, c := range []struct {
		name         string
		subprotocols []string
		haroVersion  string
		accepted     bool
		protocol     string
	}{
		{"haro 终端", []string{realtimeproto.Protocol}, "v0.6.0-aaaaaaa", true, realtimeproto.Protocol},
		{"旧版 haro 协议（v2）", []string{"haro.realtime.v2"}, "v0.5.0-aaaaaaa", false, ""},
		{"不认识的 haro 版本", []string{"haro.realtime.v9"}, "v0.9.0-aaaaaaa", false, ""},
		{"旧终端：带版本头、没报子协议", nil, "v0.5.0-aaaaaaa", false, ""},
		{"普通 Realtime 客户端", nil, "", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := newServer(t, newFakeSession(), brainserver.Options{})
			header := http.Header{}
			if c.haroVersion != "" {
				header.Set(buildinfo.Header, c.haroVersion)
			}
			conn, response, err := websocket.Dial(context.Background(), wsURL(server),
				&websocket.DialOptions{Subprotocols: c.subprotocols, HTTPHeader: header})
			if c.accepted {
				if err != nil {
					t.Fatalf("应接受，得到 %v", err)
				}
				defer conn.CloseNow()
				if conn.Subprotocol() != c.protocol {
					t.Errorf("选中的子协议 = %q，应为 %q", conn.Subprotocol(), c.protocol)
				}
				return
			}
			if err == nil {
				conn.CloseNow()
				t.Fatal("应拒绝握手")
			}
			var body realtimeproto.HTTPError
			if response == nil || response.StatusCode != http.StatusBadRequest ||
				json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&body) != nil {
				t.Fatalf("拒绝时应返回 400 和错误说明：%v %v", response, err)
			}
			if body.Error.Code != realtimeproto.CodeUnsupportedProtocolVersion ||
				len(body.Error.SupportedProtocols) != 1 || body.Error.SupportedProtocols[0] != realtimeproto.Protocol {
				t.Errorf("错误说明应带错误码和支持的版本：%+v", body.Error)
			}
		})
	}
}

// 处理事件出的错带错误码发给客户端：解码时的（不支持的事件、没声明格式就送音频）和会话返回的都一样。
func TestErrorEventsCarryCodes(t *testing.T) {
	server := newServer(t, newFakeSession(), brainserver.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(server), &websocket.DialOptions{Subprotocols: []string{realtimeproto.Protocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for _, c := range []struct {
		send string
		code realtimeproto.ErrorCode
	}{
		{`{"type":"input_audio_buffer.commit"}`, realtimeproto.CodeUnsupportedEvent},
		{`{"type":"input_audio_buffer.append","audio":"AQI="}`, realtimeproto.CodeInputFormatRequired},
		{`{"type":"response.create"}`, realtimeproto.CodeResponseInProgress},
	} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(c.send)); err != nil {
			t.Fatal(err)
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		event, err := realtimeproto.DecodeServerEvent(data)
		if failure, ok := event.(realtimeproto.Error); err != nil || !ok || failure.Code != c.code {
			t.Errorf("发 %s 应收到错误码 %s，得到 %s", c.send, c.code, data)
		}
	}
}

// 终端掉线（连接还在、但不再回应）：服务端靠心跳发现，关掉会话。
func TestSilentTerminalIsDisconnected(t *testing.T) {
	session := newFakeSession()
	server := newServer(t, session, brainserver.Options{
		Heartbeat: heartbeat.Timing{Interval: 20 * time.Millisecond, Timeout: 50 * time.Millisecond}})
	conn, _, err := websocket.Dial(context.Background(), wsURL(server),
		&websocket.DialOptions{Subprotocols: []string{realtimeproto.Protocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow() // 不读连接：WebSocket 库只在读的时候回 pong，等于终端「卡住了」
	select {
	case <-session.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("终端不回应心跳时，服务端应在间隔 + 超时后关掉会话")
	}
}

// 官方 Realtime 客户端（不带子协议）能连上：服务端解得了它的 session.update，
// 它解得了服务端的标准事件，haro 的私有事件也不会让它出错。
func TestOfficialRealtimeClientIsCompatible(t *testing.T) {
	session := newFakeSession()
	server := newServer(t, session, brainserver.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := openai.NewClient(option.WithBaseURL(server.URL+"/v1/"), option.WithAPIKey("test"), option.WithUnsafeAllowHTTP())
	conn, err := client.Realtime.Connect(ctx, realtime.RealtimeConnectionOptions{}, option.WithQuery("model", "haro"))
	if err != nil {
		t.Fatalf("官方客户端连不上：%v", err)
	}
	defer conn.Close()

	var update realtime.RealtimeClientEventUnionParam
	if err := json.Unmarshal([]byte(`{"type":"session.update","session":{"type":"realtime","audio":{"input":{`+
		`"format":{"type":"audio/pcm","rate":16000},"turn_detection":{"type":"server_vad"}}}}}`), &update); err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(ctx, update); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-session.received:
		got, ok := event.(realtimeproto.SessionUpdate)
		if !ok || got.InputFormat != realtimeproto.TranscriptionFormat || got.TurnDetection == nil {
			t.Errorf("服务端解出的 session.update = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("服务端没收到官方客户端的 session.update")
	}

	session.events <- realtimeproto.ResponseOutputAudioTranscriptDelta{Delta: "你好"}
	session.events <- realtimeproto.HaroNotice{Kind: realtimeproto.NoticeResearchDone}
	session.events <- realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	for _, want := range []struct {
		eventType string
		standard  bool
	}{
		{"response.output_audio_transcript.delta", true},
		{"haro.notice", false},
		{"response.done", true},
	} {
		event, err := conn.Recv(ctx)
		if err != nil {
			t.Fatalf("官方客户端收 %s 出错：%v", want.eventType, err)
		}
		if string(event.Type) != want.eventType {
			t.Errorf("收到 %s，应为 %s", event.Type, want.eventType)
		}
		if standard := event.AsAny() != nil; standard != want.standard {
			t.Errorf("%s 解成具体类型 = %v，应为 %v", want.eventType, standard, want.standard)
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("超时")
	}
}
