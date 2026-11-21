package speech

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
			client := &RealtimeTranscriptionClient{BaseURL: server.URL,
				Heartbeat: heartbeat.Timing{Interval: 20 * time.Millisecond, Timeout: 30 * time.Millisecond}}
			stream, err := client.OpenStream(context.Background(), serverVAD)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
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
