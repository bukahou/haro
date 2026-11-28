package graceful

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
				listener, err := net.Listen("tcp", address)
				if err != nil {
					t.Errorf("监听：%v", err)
					result <- false
					return
				}
				drained, err := Serve(ctx, listener, handler, Timing{Shutdown: time.Second, Drain: 200 * time.Millisecond}, discard)
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

// 没能提供服务（监听已经关了）：直接返回错误，还没有会话。
func TestServeReportsServeFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	drained, err := Serve(context.Background(), listener, http.NotFoundHandler(), Timing{Shutdown: time.Second, Drain: time.Second}, discard)
	if err == nil || !drained {
		t.Errorf("提供不了服务时应返回错误、没有会话：drained=%v err=%v", drained, err)
	}
}

// 两个入口：都能用；有一个提供不了服务时另一个也关掉、返回那个错误。
func TestServeAllEndpoints(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		_, err := ServeAll(ctx, []Endpoint{{Listener: first, Handler: http.NotFoundHandler()},
			{Listener: second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })}},
			Timing{Shutdown: time.Second, Drain: time.Second}, discard)
		served <- err
	}()
	for address, want := range map[string]int{first.Addr().String(): http.StatusNotFound, second.Addr().String(): http.StatusTeapot} {
		response, err := http.Get("http://" + address)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Errorf("%s：%d", address, response.StatusCode)
		}
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("正常关停：%v", err)
	}

	broken, _ := net.Listen("tcp", "127.0.0.1:0")
	broken.Close()
	working, _ := net.Listen("tcp", "127.0.0.1:0")
	if _, err := ServeAll(context.Background(), []Endpoint{{Listener: working, Handler: http.NotFoundHandler()},
		{Listener: broken, Handler: http.NotFoundHandler()}}, Timing{Shutdown: time.Second, Drain: time.Second}, discard); err == nil {
		t.Error("有一个提供不了服务时应返回错误")
	}
}
