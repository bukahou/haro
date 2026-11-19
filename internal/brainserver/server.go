// Package brainserver 把服务端挂到 WebSocket 上：
//   - GET /v1/realtime：对话会话（Realtime 事件，定义见 realtimeproto）
//   - GET /health
//
// 一条连接一个会话（与 OpenAI Realtime 一致）：连接断了，这次的对话历史就没了（性格记在文件里，不受影响）。
// 本包只管协议和连接，会话由外部注入（haro-server 注入 brain.Session）。
package brainserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Session 是一次对话会话（brain.Session 实现了它）。
type Session interface {
	Send(ctx context.Context, event realtimeproto.ClientEvent) error
	Events() <-chan realtimeproto.ServerEvent // 会话期间不关闭
	Close() error
}

// SessionFactory 为每条连接新建一个会话。
type SessionFactory func() Session

// NewHandler 返回挂好路由的 HTTP handler。
func NewHandler(newSession SessionFactory, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+realtimeproto.ConversationPath, func(w http.ResponseWriter, r *http.Request) {
		serveConversation(w, r, newSession, logger)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

func serveConversation(w http.ResponseWriter, r *http.Request, newSession SessionFactory, logger *slog.Logger) {
	w.Header().Set(buildinfo.Header, buildinfo.Version) // 握手响应里报服务端的版本（只用于日志）
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logger.Warn("WebSocket 握手失败", "remote", r.RemoteAddr, "err", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(realtimeproto.MaxMessageBytes)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	session := newSession()
	defer session.Close()
	if terminalVersion := r.Header.Get(buildinfo.Header); buildinfo.Mismatch(buildinfo.Version, terminalVersion) {
		logger.Warn("终端已连接，但版本和服务端不同：在那台终端上重新运行安装脚本升到最新", "remote", r.RemoteAddr,
			"terminal", buildinfo.Reported(terminalVersion), "server", buildinfo.Version)
	} else {
		logger.Info("终端已连接", "remote", r.RemoteAddr, "version", buildinfo.Reported(terminalVersion))
	}

	var writeMu sync.Mutex
	write := func(event realtimeproto.ServerEvent) error {
		data, err := realtimeproto.EncodeServerEvent(event)
		if err != nil {
			logger.Error("编码事件失败", "event", event.Type(), "err", err)
			return nil
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.Write(ctx, websocket.MessageText, data)
	}
	go func() { // 会话的事件 → 终端
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-session.Events():
				if err := write(event); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	var inputFormat realtimeproto.AudioFormat // session.update 声明之前为空：这时送来的音频会被拒绝
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if status := websocket.CloseStatus(err); status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway ||
				errors.Is(err, io.EOF) || ctx.Err() != nil {
				logger.Info("终端已断开", "remote", r.RemoteAddr)
			} else {
				logger.Warn("终端连接异常断开", "remote", r.RemoteAddr, "err", err)
			}
			return
		}
		event, err := realtimeproto.DecodeClientEvent(data, inputFormat)
		if err != nil {
			_ = write(realtimeproto.ErrorEventFor("", err))
			continue
		}
		if update, ok := event.(realtimeproto.SessionUpdate); ok && update.InputFormat.SampleRate > 0 {
			inputFormat = update.InputFormat
		}
		if err := session.Send(ctx, event); err != nil {
			_ = write(realtimeproto.ErrorEventFor(event.Type(), err))
		}
	}
}
