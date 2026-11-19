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

// Serve 在 listener 上提供 handler，直到 ctx 结束；然后按顺序关停。返回会话是不是都在期限内退出了。
// 没能提供服务（例如监听出错）时返回那个错误，这时还没有会话。
func Serve(ctx context.Context, listener net.Listener, handler http.Handler, timing Timing, logger *slog.Logger) (drained bool, err error) {
	sessionsCtx, endSessions := context.WithCancel(context.Background())
	defer endSessions()
	var sessions sync.WaitGroup
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sessions.Add(1)
			defer sessions.Done()
			handler.ServeHTTP(w, r)
		}),
		BaseContext:       func(net.Listener) context.Context { return sessionsCtx },
		ReadHeaderTimeout: 10 * time.Second,
	}
	serving := make(chan error, 1)
	go func() { serving <- server.Serve(listener) }()
	select {
	case err := <-serving: // 没能提供服务：还没有会话
		return true, err
	case <-ctx.Done():
	}

	logger.Info("收到退出信号：停止接新连接，结束进行中的会话")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timing.Shutdown)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	endSessions()
	finished := make(chan struct{})
	go func() {
		sessions.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		logger.Info("会话都已结束")
		return true, nil
	case <-time.After(timing.Drain):
		logger.Warn("会话没在期限内结束：照样往下关", "timeout", timing.Drain)
		return false, nil
	}
}
