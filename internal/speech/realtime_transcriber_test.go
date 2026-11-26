package speech

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/heartbeat"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// fakeTranscriptionServer 是替身转写服务：读到 session.update 后照 reply 答复（nil = 不答），再交给 then 接着处理连接。
func fakeTranscriptionServer(t *testing.T, reply realtimeproto.ServerEvent,
	then func(ctx context.Context, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil { // session.update
			return
		}
		if reply != nil {
			data, err := realtimeproto.EncodeServerEvent(reply)
			if err != nil {
				t.Error(err)
				return
			}
			if err := conn.Write(r.Context(), websocket.MessageText, data); err != nil {
				return
			}
		}
		if then != nil {
			then(r.Context(), conn)
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server
}

// accepted 是转写服务接受设置时的答复。
var accepted = realtimeproto.SessionUpdated{Session: realtimeproto.SessionUpdate{Kind: realtimeproto.SessionTranscription,
	TurnDetection: &realtimeproto.TurnDetection{Type: realtimeproto.TurnServerVAD}, InputFormat: realtimeproto.TranscriptionFormat}}

var serverVAD = brain.StreamConfig{TurnDetection: realtimeproto.TurnDetection{Type: realtimeproto.TurnServerVAD}}

// readUntilClosed 一直读（库会自动回 pong），直到连接断开。
func readUntilClosed(ctx context.Context, conn *websocket.Conn) {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}

// 转写服务卡住（连接还在、不再读，也就不回 ping）：心跳等不到回应就断开，事件通道关闭，会话据此重开。
// 对照：同样的服务照常读（会回 ping）时，连接一直在。
func TestTranscriptionStreamDropsUnresponsiveServer(t *testing.T) {
	for _, c := range []struct {
		name      string
		stuck     bool
		wantClose bool
	}{{"卡住", true, true}, {"照常（阳性对照）", false, false}} {
		t.Run(c.name, func(t *testing.T) {
			server := fakeTranscriptionServer(t, accepted, func(ctx context.Context, conn *websocket.Conn) {
				if c.stuck {
					<-ctx.Done() // 不再读：ping 没人回
					return
				}
				readUntilClosed(ctx, conn)
			})
			client := &RealtimeTranscriptionClient{BaseURL: server.URL, Logger: slog.New(slog.DiscardHandler),
				Heartbeat: heartbeat.Timing{Interval: 20 * time.Millisecond, Timeout: 30 * time.Millisecond}}
			stream, err := client.OpenStream(context.Background(), serverVAD)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			expectAccepted(t, stream)
			select {
			case _, open := <-stream.Events():
				if open || !c.wantClose {
					t.Errorf("事件通道：open=%v，应关闭=%v", open, c.wantClose)
				}
			case <-time.After(500 * time.Millisecond):
				if c.wantClose {
					t.Error("对端卡住时应在心跳超时后断开")
				}
			}
		})
	}
}

// 打开识别流要等到转写服务确认（session.updated）：确认之前不返回，一直不确认就在 ctx 的期限上失败
// （拨通、发出设置不算打开了）。
func TestOpenStreamWaitsForSessionUpdated(t *testing.T) {
	release := make(chan struct{})
	slow := fakeTranscriptionServer(t, nil, func(ctx context.Context, conn *websocket.Conn) {
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		data, _ := realtimeproto.EncodeServerEvent(accepted)
		_ = conn.Write(ctx, websocket.MessageText, data)
		readUntilClosed(ctx, conn)
	})
	opened := make(chan error, 1)
	go func() {
		stream, err := NewRealtimeTranscriptionClient(slow.URL).OpenStream(context.Background(), serverVAD)
		if err == nil {
			defer stream.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("确认之前不应返回：%v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("确认之后应打开：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("确认之后应马上返回")
	}

	silent := fakeTranscriptionServer(t, nil, nil) // 收下设置、一直不答
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if stream, err := NewRealtimeTranscriptionClient(silent.URL).OpenStream(ctx, serverVAD); err == nil {
		stream.Close()
		t.Fatal("一直不确认时不应算打开了")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("一直不确认时应在 ctx 的期限上失败：等了 %v", elapsed)
	}
}

// logRecords 记下所有日志（测试按级别和属性核对，不看日志的文字）。
type logRecords struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecords) Handle(_ context.Context, record slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, record.Clone())
	return nil
}
func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecords) WithGroup(string) slog.Handler      { return l }

// warnings 是带 key 属性的警告级日志，每条给出全部属性。
func (l *logRecords) warnings(key string) []map[string]slog.Value {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []map[string]slog.Value
	for _, record := range l.records {
		attrs := map[string]slog.Value{}
		record.Attrs(func(attr slog.Attr) bool {
			attrs[attr.Key] = attr.Value
			return true
		})
		if _, ok := attrs[key]; ok && record.Level == slog.LevelWarn {
			found = append(found, attrs)
		}
	}
	return found
}

// drain 读完识别流的事件，直到事件通道关闭。
func drain(t *testing.T, stream brain.RecognizerStream) []realtimeproto.ServerEvent {
	t.Helper()
	var events []realtimeproto.ServerEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, open := <-stream.Events():
			if !open {
				return events
			}
			events = append(events, event)
		case <-timeout:
			t.Fatalf("事件通道一直没关：已收到 %v", events)
			return nil
		}
	}
}

// 转写服务发来解不开的事件：跳过，同一种只在第一次记一条警告（不每条刷一行），流结束时报这一种一共几条；
// 解得开的照常交给会话。连接被对端断开时记一条警告写明原因。
func TestTranscriptionStreamLogsUndecodableEvents(t *testing.T) {
	const speechStarted = "input_audio_buffer.speech_started"
	server := fakeTranscriptionServer(t, accepted, func(ctx context.Context, conn *websocket.Conn) {
		for range 3 {
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"`+speechStarted+`","audio_start_ms":"不是数"}`))
		}
		valid, _ := realtimeproto.EncodeServerEvent(realtimeproto.InputAudioBufferSpeechStarted{ItemID: "item_1"})
		_ = conn.Write(ctx, websocket.MessageText, valid)
		conn.Close(websocket.StatusNormalClosure, "")
	})
	logs := &logRecords{}
	client := &RealtimeTranscriptionClient{BaseURL: server.URL, Logger: slog.New(logs)}
	stream, err := client.OpenStream(context.Background(), serverVAD)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	expectAccepted(t, stream)
	if events := drain(t, stream); len(events) != 1 {
		t.Errorf("解得开的那一个应照常交给会话：%v", events)
	}
	var first, summaries int
	for _, attrs := range logs.warnings("event") {
		if attrs["event"].String() != speechStarted {
			t.Errorf("警告里的事件名不对：%v", attrs)
		}
		if _, ok := attrs["err"]; ok {
			first++
		}
		if count, ok := attrs["count"]; ok && count.Int64() == 3 {
			summaries++
		}
	}
	if first != 1 || summaries != 1 {
		t.Errorf("同一种解不开的事件应只在第一次警告一条（%d 条）、结束时报一共 3 条（%d 条）", first, summaries)
	}
	if len(logs.warnings("reason")) != 1 {
		t.Errorf("对端断开连接时应记一条写明原因的警告：%v", logs.warnings("reason"))
	}
}

// 连接为什么结束：心跳等不到回应时，警告里的原因是心跳断开；由 Close 关掉的不是断开，不警告。
func TestTranscriptionStreamEndReason(t *testing.T) {
	stuck := fakeTranscriptionServer(t, accepted, func(ctx context.Context, _ *websocket.Conn) { <-ctx.Done() })
	logs := &logRecords{}
	client := &RealtimeTranscriptionClient{BaseURL: stuck.URL, Logger: slog.New(logs),
		Heartbeat: heartbeat.Timing{Interval: 20 * time.Millisecond, Timeout: 30 * time.Millisecond}}
	stream, err := client.OpenStream(context.Background(), serverVAD)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, stream)
	stream.Close()
	reasons := logs.warnings("reason")
	if len(reasons) != 1 {
		t.Fatalf("心跳断开应记一条写明原因的警告：%v", reasons)
	}
	if reason, ok := reasons[0]["reason"].Any().(error); !ok || !errors.Is(reason, errHeartbeatLost) {
		t.Errorf("原因应是心跳断开：%v", reasons[0]["reason"])
	}

	normal := fakeTranscriptionServer(t, accepted, readUntilClosed)
	logs = &logRecords{}
	client = &RealtimeTranscriptionClient{BaseURL: normal.URL, Logger: slog.New(logs)}
	stream, err = client.OpenStream(context.Background(), serverVAD)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	drain(t, stream)
	if reasons := logs.warnings("reason"); len(reasons) != 0 {
		t.Errorf("由 Close 关掉的不是断开，不应警告：%v", reasons)
	}
}

// 转写服务不接受设置：请求类的码 ⇒ ErrStreamRejected（原样再连没用），码原样带出来；
// 它自己的问题（server_error 类）⇒ 普通的错误（会话照常重开），码同样带出来。
func TestOpenStreamRefusals(t *testing.T) {
	for _, c := range []struct {
		name     string
		code     realtimeproto.ErrorCode
		rejected bool
	}{
		{"做不到的设置", realtimeproto.CodeUnsupportedFeature, true},
		{"取值不对", realtimeproto.CodeInvalidEvent, true},
		{"转写服务自己出错", realtimeproto.CodeServerError, false},
		{"转写服务的会话满了", realtimeproto.CodeTooManySessions, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := fakeTranscriptionServer(t, realtimeproto.Error{Code: c.code, Message: "不行"}, nil)
			stream, err := NewRealtimeTranscriptionClient(server.URL).OpenStream(context.Background(), serverVAD)
			if err == nil {
				stream.Close()
				t.Fatal("不接受时不应打开")
			}
			if rejected := errors.Is(err, brain.ErrStreamRejected); rejected != c.rejected {
				t.Errorf("ErrStreamRejected = %v，应为 %v：%v", rejected, c.rejected, err)
			}
			if refusal, ok := errors.AsType[*realtimeproto.ProtocolError](err); !ok || refusal.Code != c.code {
				t.Errorf("应带出转写服务的码 %s：%v", c.code, err)
			}
		})
	}
}

// expectAccepted 读出打开时排在最前面的那个事件：转写服务确认设置的 session.updated。
func expectAccepted(t *testing.T, stream brain.RecognizerStream) realtimeproto.SessionUpdated {
	t.Helper()
	select {
	case event := <-stream.Events():
		updated, ok := event.(realtimeproto.SessionUpdated)
		if !ok {
			t.Fatalf("打开后第一个事件应是转写服务的 session.updated，得到 %#v", event)
		}
		return updated
	case <-time.After(time.Second):
		t.Fatal("打开后应先交出转写服务的 session.updated")
	}
	return realtimeproto.SessionUpdated{}
}

// 转写服务确认设置时报的唤醒词原样交给会话（排在所有事件前面）：会话转给终端，唤醒词只在关键词文件里写一处。
func TestTranscriptionStreamPassesAcceptedSettings(t *testing.T) {
	reply := realtimeproto.SessionUpdated{Session: realtimeproto.SessionUpdate{Kind: realtimeproto.SessionTranscription,
		TurnDetection: &realtimeproto.TurnDetection{Type: "server_vad"}, WakeWord: true, WakePhrases: []string{"哈罗哈罗"},
		InputFormat: realtimeproto.TranscriptionFormat}}
	server := fakeTranscriptionServer(t, reply, func(ctx context.Context, conn *websocket.Conn) {
		started, _ := realtimeproto.EncodeServerEvent(realtimeproto.InputAudioBufferSpeechStarted{ItemID: "item_1"})
		_ = conn.Write(ctx, websocket.MessageText, started)
		readUntilClosed(ctx, conn)
	})
	client := &RealtimeTranscriptionClient{BaseURL: server.URL, Logger: slog.New(slog.DiscardHandler)}
	stream, err := client.OpenStream(context.Background(), serverVAD)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if got := expectAccepted(t, stream); !reflect.DeepEqual(got.Session.WakePhrases, []string{"哈罗哈罗"}) {
		t.Errorf("唤醒词应原样交给会话：%q", got.Session.WakePhrases)
	}
	select {
	case event := <-stream.Events():
		if _, ok := event.(realtimeproto.InputAudioBufferSpeechStarted); !ok {
			t.Errorf("之后的事件照常交出：%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("之后的事件没交出来")
	}
}
