// Package graceful 让 HTTP 服务（含 WebSocket 这类长连接）按顺序关停：
// 收到退出信号 → 停止接新连接（等普通请求做完）→ 结束进行中的会话 → 等它们退出。
//
// http.Server.Shutdown 不管被接管的连接（WebSocket）：会话的 context 由 BaseContext 派生，关停时取消它，会话才会结束；
// 处理函数返回前会话已经收尾（例如对话这一轮已经落盘），所以之后再关数据库不会和它们抢。
package graceful

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Timing 是关停各步的期限。
type Timing struct {
	Shutdown time.Duration // 停止接新连接后，等普通请求做完最多多久
	Drain    time.Duration // 取消会话后，等它们退出最多多久
}

// Endpoint 是一个监听地址和它上面的 handler（例如局域网的入口、公网的入口）。
type Endpoint struct {
	Listener net.Listener
	Handler  http.Handler
}

// Serve 在 listener 上提供 handler，直到 ctx 结束；然后按顺序关停。返回会话是不是都在期限内退出了。
// 没能提供服务（例如监听出错）时返回那个错误，这时还没有会话。
func Serve(ctx context.Context, listener net.Listener, handler http.Handler, timing Timing, logger *slog.Logger) (drained bool, err error) {
	return ServeAll(ctx, []Endpoint{{Listener: listener, Handler: handler}}, timing, logger)
}

// ServeAll 同 Serve，同时提供几个入口：一起停止接新连接、一起结束会话；有一个没能提供服务就全部关掉、返回那个错误。
func ServeAll(ctx context.Context, endpoints []Endpoint, timing Timing, logger *slog.Logger) (drained bool, err error) {
	sessionsCtx, endSessions := context.WithCancel(context.Background())
	defer endSessions()
	var sessions sync.WaitGroup
	servers := make([]*http.Server, 0, len(endpoints))
	serving := make(chan error, len(endpoints))
	for _, endpoint := range endpoints {
		handler := endpoint.Handler
		server := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sessions.Add(1)
				defer sessions.Done()
				handler.ServeHTTP(w, r)
			}),
			BaseContext:       func(net.Listener) context.Context { return sessionsCtx },
			ReadHeaderTimeout: 10 * time.Second,
		}
		servers = append(servers, server)
		go func(listener net.Listener) { serving <- server.Serve(listener) }(endpoint.Listener)
	}
	var failed error
	select {
	case failed = <-serving: // 有一个没能提供服务：其余的也关掉
	case <-ctx.Done():
	}

	logger.Info("收到退出信号：停止接新连接，结束进行中的会话")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timing.Shutdown)
	defer cancel()
	for _, server := range servers {
		_ = server.Shutdown(shutdownCtx)
	}
	endSessions()
	finished := make(chan struct{})
	go func() {
		sessions.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		logger.Info("会话都已结束")
		return true, failed
	case <-time.After(timing.Drain):
		logger.Warn("会话没在期限内结束：照样往下关", "timeout", timing.Drain)
		return false, failed
	}
}
