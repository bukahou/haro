package brainclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/brainclient"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/heartbeat"
	"github.com/bukahou/haro/internal/realtimeproto"
)

var (
	mic16k   = realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}
	voice32k = realtimeproto.AudioFormat{SampleRate: 32000, Channels: 1}
	quiet    = slog.New(slog.NewTextHandler(io.Discard, nil))
)

// scriptedSession 是假的服务端会话：记下收到的事件；收到 response.create 就按顺序发出 reply，
// 再发 response.done；hang 时发完 reply 不结束，直到会话关闭（模拟还在生成）。
type scriptedSession struct {
	reply  []realtimeproto.ServerEvent
	hang   bool
	events chan realtimeproto.ServerEvent
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	received []realtimeproto.ClientEvent
}

func newScriptedSession(hang bool, reply ...realtimeproto.ServerEvent) *scriptedSession {
	return &scriptedSession{reply: reply, hang: hang, events: make(chan realtimeproto.ServerEvent, 64),
		closed: make(chan struct{})}
}

func (s *scriptedSession) Send(_ context.Context, event realtimeproto.ClientEvent) error {
	s.mu.Lock()
	s.received = append(s.received, event)
	s.mu.Unlock()
	if _, ok := event.(realtimeproto.ResponseCreate); ok {
		go s.respond()
	}
	return nil
}

func (s *scriptedSession) respond() {
	events := append([]realtimeproto.ServerEvent(nil), s.reply...)
	if !s.hang {
		events = append(events, realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted})
	}
	for _, event := range events {
		select {
		case s.events <- event:
		case <-s.closed:
			return
		}
	}
}

func (s *scriptedSession) Events() <-chan realtimeproto.ServerEvent { return s.events }
func (s *scriptedSession) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *scriptedSession) receivedEvents() []realtimeproto.ClientEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]realtimeproto.ClientEvent(nil), s.received...)
}

// newServer 起一个只接这条假会话的服务端（每个测试只连一次）。
func newServer(session *scriptedSession) *httptest.Server {
	return httptest.NewServer(brainserver.NewHandler(func() brainserver.Session { return session }, brainserver.Options{}, quiet))
}

// askOverWebSocket 发出一轮的客户端事件：打开会话（服务端判断说完）、送一段音频、带上画面、请求回答。
func askOverWebSocket(t *testing.T, client *brainclient.Client) {
	t.Helper()
	ctx := context.Background()
	turn := realtimeproto.TurnDetection{Type: "server_vad"}
	for _, event := range []realtimeproto.ClientEvent{
		realtimeproto.SessionUpdate{TurnDetection: &turn}, // 输入格式由客户端补上
		realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: mic16k, Data: make([]byte, 3200)}},
		realtimeproto.ConversationItemCreate{Item: realtimeproto.ConversationItem{Role: "user",
			Content: []realtimeproto.ContentPart{{Type: "input_image", ImageJPEG: []byte{0xFF, 0xD8}}}}},
		realtimeproto.ResponseCreate{},
	} {
		if err := client.Send(ctx, event); err != nil {
			t.Fatalf("发送 %s: %v", event.Type(), err)
		}
	}
}

func collectUntilDone(t *testing.T, client *brainclient.Client) []realtimeproto.ServerEvent {
	t.Helper()
	var events []realtimeproto.ServerEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-client.Events():
			events = append(events, event)
			if _, done := event.(realtimeproto.ResponseDone); done {
				return events
			}
		case <-timeout:
			t.Fatalf("等 response.done 超时，已收到 %v", events)
		}
	}
}

// 一轮完整的对话走 WebSocket：客户端事件原样到达服务端（输入格式由客户端补上）；
// 识别结果、逐句文字、分段的 32 kHz 声音、response.done 原样到达终端。
func TestConversationOverWebSocket(t *testing.T) {
	session := newScriptedSession(false,
		realtimeproto.InputAudioTranscriptionCompleted{ItemID: "item_1", Transcript: "讲个冷笑话"},
		realtimeproto.ResponseOutputAudioTranscriptDelta{Delta: "为什么数学老师离婚了？"},
		realtimeproto.ResponseOutputAudioDelta{Audio: realtimeproto.PCMAudio{Format: voice32k, Data: []byte("前半")}},
		realtimeproto.ResponseOutputAudioDelta{Audio: realtimeproto.PCMAudio{Format: voice32k, Data: []byte("后半")}},
		realtimeproto.ResponseOutputAudioTranscriptDelta{Delta: "因为解不开。"},
		realtimeproto.ResponseOutputAudioDelta{Audio: realtimeproto.PCMAudio{Format: voice32k, Data: []byte("前半")}},
		realtimeproto.ResponseOutputAudioDelta{Audio: realtimeproto.PCMAudio{Format: voice32k, Data: []byte("后半")}},
	)
	server := newServer(session)
	defer server.Close()
	client, err := brainclient.Dial(context.Background(), brainclient.DialOptions{BaseURL: server.URL, InputFormat: mic16k}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	askOverWebSocket(t, client)
	events := collectUntilDone(t, client)

	var transcript strings.Builder
	audioDeltas := 0
	for _, event := range events {
		switch e := event.(type) {
		case realtimeproto.InputAudioTranscriptionCompleted:
			if e.Transcript != "讲个冷笑话" {
				t.Errorf("识别结果 %q", e.Transcript)
			}
		case realtimeproto.ResponseOutputAudioTranscriptDelta:
			transcript.WriteString(e.Delta)
		case realtimeproto.ResponseOutputAudioDelta:
			if e.Audio.Format != voice32k {
				t.Errorf("音频格式应原样带过来：%+v", e.Audio.Format)
			}
			audioDeltas++
		case realtimeproto.Error:
			t.Errorf("不应出错：%s", e.Message)
		}
	}
	if transcript.String() != "为什么数学老师离婚了？因为解不开。" {
		t.Errorf("文字 = %q", transcript.String())
	}
	if audioDeltas != 4 {
		t.Errorf("声音应分段到达，收到 %d 段", audioDeltas)
	}
	if done := events[len(events)-1].(realtimeproto.ResponseDone); done.Status != realtimeproto.StatusCompleted {
		t.Errorf("response.done = %+v", done)
	}

	received := session.receivedEvents()
	if len(received) != 4 {
		t.Fatalf("服务端应收到 4 个事件，收到 %d 个", len(received))
	}
	if update, ok := received[0].(realtimeproto.SessionUpdate); !ok || update.InputFormat != mic16k || update.TurnDetection == nil {
		t.Errorf("session.update 应带上客户端补的输入格式和 turn_detection：%+v", received[0])
	}
	if audio, ok := received[1].(realtimeproto.InputAudioBufferAppend); !ok || audio.Audio.Format != mic16k || len(audio.Audio.Data) != 3200 {
		t.Errorf("音频应原样到达：%+v", received[1])
	}
}

// trackingListener 记下每条底层连接，killAll 一次性关掉（模拟服务端进程挂掉；httptest 管不到已升级成 WebSocket 的连接）。
type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, conn)
		l.mu.Unlock()
	}
	return conn, err
}

func (l *trackingListener) killAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.conns {
		_ = conn.Close()
	}
}

// 回答到一半服务端断了：终端收到 error + 失败的 response.done（不会一直等），之后 Send 返回 ErrDisconnected。
func TestDisconnectDuringResponse(t *testing.T) {
	server := newServer(newScriptedSession(true, realtimeproto.ResponseOutputAudioTranscriptDelta{Delta: "说到一半"}))
	server.Close() // 只借用它的 handler，改用自己的监听
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &trackingListener{Listener: raw}
	httpServer := &http.Server{Handler: server.Config.Handler}
	go httpServer.Serve(listener)
	defer httpServer.Close()

	client, err := brainclient.Dial(context.Background(), brainclient.DialOptions{BaseURL: "http://" + raw.Addr().String(), InputFormat: mic16k}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	askOverWebSocket(t, client)
	time.Sleep(100 * time.Millisecond)
	listener.killAll()

	events := collectUntilDone(t, client)
	sawError := false
	for _, event := range events {
		if _, ok := event.(realtimeproto.Error); ok {
			sawError = true
		}
	}
	done := events[len(events)-1].(realtimeproto.ResponseDone)
	if !sawError || done.Status != realtimeproto.StatusFailed || done.Reason != realtimeproto.ReasonServerDisconnected {
		t.Errorf("应收到 error 和失败的 response.done：%v", events)
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("Done() 应该关闭")
	}
	if err := client.Send(context.Background(), realtimeproto.ResponseCreate{}); !errors.Is(err, brainclient.ErrDisconnected) {
		t.Errorf("断开后 Send 应返回 ErrDisconnected，得到 %v", err)
	}
}

// 服务端没起来：只连一次，马上返回 ErrUnreachable（重试由看守负责）。
func TestDialReportsUnreachable(t *testing.T) {
	server := newServer(newScriptedSession(false))
	url := server.URL
	server.Close() // 地址存在但没人监听
	_, err := brainclient.Dial(context.Background(), brainclient.DialOptions{BaseURL: url, InputFormat: mic16k}, quiet)
	if !errors.Is(err, brainclient.ErrUnreachable) || errors.Is(err, realtimeproto.ErrIncompatibleProtocol) {
		t.Fatalf("应返回 ErrUnreachable，得到 %v", err)
	}
}

// 协议版本不兼容：服务端拒绝握手（例如它只认更新的版本），或者服务端是旧版、没有选 haro 的子协议。
// 两种都返回 ErrIncompatibleProtocol，看守据此不再频繁重连。
func TestDialDetectsIncompatibleProtocol(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := realtimeproto.NewHTTPError(http.StatusBadRequest, realtimeproto.CodeUnsupportedProtocolVersion, "只支持 v9")
		body.Error.SupportedProtocols = []string{"haro.realtime.v9"}
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer rejecting.Close()
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil) // 旧版服务端不认子协议
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
	}))
	defer oldServer.Close()
	for name, url := range map[string]string{"服务端拒绝": rejecting.URL, "旧版服务端": oldServer.URL} {
		_, err := brainclient.Dial(context.Background(), brainclient.DialOptions{BaseURL: url, InputFormat: mic16k}, quiet)
		if !errors.Is(err, realtimeproto.ErrIncompatibleProtocol) {
			t.Errorf("%s：应返回 ErrIncompatibleProtocol，得到 %v", name, err)
		}
	}
}

// 服务端掉线（连接还在、但不再回应）：终端靠心跳发现，补发 error 并关闭 Done()。
func TestSilentServerIsDetected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{realtimeproto.ProtocolV2}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		<-r.Context().Done() // 不读连接：WebSocket 库只在读的时候回 pong
	}))
	defer server.Close()
	client, err := brainclient.Dial(context.Background(), brainclient.DialOptions{BaseURL: server.URL, InputFormat: mic16k,
		Heartbeat: heartbeat.Timing{Interval: 20 * time.Millisecond, Timeout: 50 * time.Millisecond}}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("服务端不回应心跳时，终端应在间隔 + 超时后断开")
	}
	select {
	case event := <-client.Events():
		if _, ok := event.(realtimeproto.Error); !ok {
			t.Errorf("断开时应补发 error，收到 %#v", event)
		}
	case <-time.After(time.Second):
		t.Error("断开时应补发 error")
	}
}

// 音频格式和声明的不一致：直接拒绝（避免服务端按错的采样率去识别）。
func TestRejectsMismatchedAudioFormat(t *testing.T) {
	server := newServer(newScriptedSession(false))
	defer server.Close()
	client, err := brainclient.Dial(context.Background(), brainclient.DialOptions{BaseURL: server.URL, InputFormat: mic16k}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.Send(context.Background(), realtimeproto.InputAudioBufferAppend{Audio: realtimeproto.PCMAudio{Format: voice32k, Data: []byte{1, 2}}})
	if !errors.Is(err, brainclient.ErrFormatMismatch) {
		t.Errorf("格式不一致应返回 ErrFormatMismatch，得到 %v", err)
	}
}
