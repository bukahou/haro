package speech

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/heartbeat"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 转写服务卡住（连接还在、不再读，也就不回 ping）：心跳等不到回应就断开，事件通道关闭，会话据此重开。
// 对照：同样的服务照常读（会回 ping）时，连接一直在。
func TestTranscriptionStreamDropsUnresponsiveServer(t *testing.T) {
	for _, c := range []struct {
		name      string
		stuck     bool
		wantClose bool
	}{{"卡住", true, true}, {"照常（阳性对照）", false, false}} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if _, _, err := conn.Read(r.Context()); err != nil { // session.update
					return
				}
				if c.stuck {
					<-r.Context().Done() // 不再读：ping 没人回
					return
				}
				for { // 照常读：库会自动回 pong
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			client := &RealtimeTranscriptionClient{BaseURL: server.URL,
				Heartbeat: heartbeat.Timing{Interval: 20 * time.Millisecond, Timeout: 30 * time.Millisecond}}
			stream, err := client.OpenStream(context.Background(), brain.StreamConfig{TurnDetection: realtimeproto.TurnDetection{Type: "server_vad"}})
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
