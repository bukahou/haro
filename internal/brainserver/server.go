// Package brainserver 把服务端挂到 WebSocket 上：
//   - GET /v1/realtime：对话会话（Realtime 事件，定义见 realtimeproto）
//   - GET /health
//
// 一条连接一个会话（与 OpenAI Realtime 一致）：连接断了，这次的对话历史就没了（性格记在文件里，不受影响）。
// 本包只管协议和连接，会话由外部注入（haro-server 注入 brain.Session）。
//
// 握手时按 WebSocket 子协议协商版本（negotiate）：
//   - 报了 realtimeproto.ProtocolV2：haro 终端；
//   - 报了别的 haro.realtime.* 版本，或者带着 haro 版本头却没报子协议（旧版终端）：拒绝握手，响应里给错误码和支持的版本；
//   - 什么都没报：普通的 Realtime 客户端，只用标准事件。
package brainserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/heartbeat"
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

// Options 是连接层的设置。零值可用。
type Options struct {
	Heartbeat heartbeat.Timing // 零值 = heartbeat.Default
}

// haroProtocolPrefix 是 haro 各版本子协议名的共同开头。
const haroProtocolPrefix = "haro.realtime."

// NewHandler 返回挂好路由的 HTTP handler。
func NewHandler(newSession SessionFactory, opts Options, logger *slog.Logger) http.Handler {
	opts.Heartbeat = opts.Heartbeat.OrDefault()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+realtimeproto.ConversationPath, func(w http.ResponseWriter, r *http.Request) {
		serveConversation(w, r, newSession, opts, logger)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

// negotiate 按终端在握手里报的子协议决定接不接这条连接；拒绝时 reason 说明原因。
func negotiate(r *http.Request) (reason string, ok bool) {
	var offered []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(header, ",") {
			offered = append(offered, strings.TrimSpace(protocol))
		}
	}
	haroOffered := slices.ContainsFunc(offered, func(protocol string) bool {
		return strings.HasPrefix(protocol, haroProtocolPrefix)
	})
	switch {
	case slices.Contains(offered, realtimeproto.ProtocolV2):
		return "", true
	case haroOffered:
		return "终端报的协议版本（" + strings.Join(offered, ", ") + "）服务端不支持", false
	case r.Header.Get(buildinfo.Header) != "":
		return "终端是旧版本（握手没有报协议版本）", false
	default:
		return "", true
	}
}

// reject 拒绝握手：400，响应体带错误码和服务端支持的版本。
func reject(w http.ResponseWriter, reason string) {
	body := realtimeproto.NewHTTPError(http.StatusBadRequest, realtimeproto.CodeUnsupportedProtocolVersion,
		reason+"：把终端和服务端升到同一版本")
	body.Error.SupportedProtocols = []string{realtimeproto.ProtocolV2}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(body)
}

func serveConversation(w http.ResponseWriter, r *http.Request, newSession SessionFactory, opts Options, logger *slog.Logger) {
	w.Header().Set(buildinfo.Header, buildinfo.Version) // 握手响应里报服务端的版本（只用于日志）
	terminalVersion := r.Header.Get(buildinfo.Header)
	if reason, ok := negotiate(r); !ok {
		logger.Warn("拒绝连接：协议版本不兼容", "remote", r.RemoteAddr, "reason", reason,
			"terminal", buildinfo.Reported(terminalVersion), "server", buildinfo.Version)
		reject(w, reason)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{realtimeproto.ProtocolV2}})
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
	protocol := conn.Subprotocol()
	if protocol == "" {
		logger.Info("普通 Realtime 客户端已连接（只用标准事件）", "remote", r.RemoteAddr)
	} else if buildinfo.Mismatch(buildinfo.Version, terminalVersion) {
		logger.Warn("终端已连接，但版本和服务端不同：在那台终端上重新运行安装脚本升到最新", "remote", r.RemoteAddr,
			"terminal", buildinfo.Reported(terminalVersion), "server", buildinfo.Version)
	} else {
		logger.Info("终端已连接", "remote", r.RemoteAddr, "version", buildinfo.Reported(terminalVersion), "protocol", protocol)
	}

	go func() { // 心跳：对端掉线（半开连接）时尽快发现，关掉会话
		if err := heartbeat.Keep(ctx, conn, opts.Heartbeat); err != nil {
			logger.Warn("终端没有回应心跳：关掉连接", "remote", r.RemoteAddr, "err", err)
			cancel()
		}
	}()

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
