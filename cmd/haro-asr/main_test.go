package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// freeAddress 找一个本机空闲端口。
func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// 关停时：WebSocket 会话（被接管的连接，Shutdown 不等它）只看请求 context 的，会被结束，serve 报告会话都退出了（之后才能释放模型）；
// 不理会 context 的会话（阳性对照）等到期限还没退出，serve 报告没退完（不释放模型）。
func TestServeEndsSessionsBeforeReturning(t *testing.T) {
	for _, c := range []struct {
		name        string
		honorCtx    bool
		wantDrained bool
	}{{"会话跟着 context 结束", true, true}, {"会话不理 context（阳性对照）", false, false}} {
		t.Run(c.name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				close(started)
				if c.honorCtx {
					_, _, _ = conn.Read(r.Context()) // 像转写会话那样：一直读，直到请求 context 结束
					return
				}
				<-release
			})
			address := freeAddress(t)
			ctx, signal := context.WithCancel(context.Background())
			result := make(chan bool, 1)
			go func() {
				drained, err := serve(ctx, address, handler, 200*time.Millisecond, discard)
				if err != nil {
					t.Errorf("serve: %v", err)
				}
				result <- drained
			}()
			var client *websocket.Conn
			for range 100 { // 等它开始监听
				var err error
				if client, _, err = websocket.Dial(context.Background(), "ws://"+address+"/", nil); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if client == nil {
				t.Fatal("连不上")
			}
			defer client.CloseNow()
			<-started
			signal() // 收到退出信号
			select {
			case drained := <-result:
				if drained != c.wantDrained {
					t.Errorf("会话都退出了 = %v，应为 %v", drained, c.wantDrained)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("serve 没在期限内返回")
			}
		})
	}
}

// 没能监听（端口被占）：直接返回错误，还没有会话（可以释放模型）。
func TestServeReportsListenFailure(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	drained, err := serve(context.Background(), busy.Addr().String(), http.NotFoundHandler(), time.Second, discard)
	if err == nil || !drained {
		t.Errorf("端口被占时应返回错误、可以释放模型：drained=%v err=%v", drained, err)
	}
}
