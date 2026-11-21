// Package brainserver 把服务端挂到 WebSocket 上：
//   - GET /v1/realtime：对话会话（Realtime 事件，定义见 realtimeproto）
//
// 存活 / 就绪检查（/livez、/readyz）由组装处挂在同一个 HTTP 服务上（见 internal/readiness）。
//
// 一条连接一个会话（与 OpenAI Realtime 一致）；对话跟人走、存在对话库里，换连接、重连、服务端重启都接得上（见 haro-server 的组装）。
// 本包只管协议和连接，会话由外部注入（haro-server 注入 brain.Session）。
//
// 握手时按 WebSocket 子协议协商版本（negotiate）：
//   - 报了 realtimeproto.Protocol：haro 终端；
//   - 报了别的 haro.realtime.* 版本，或者带着 haro 版本头却没报子协议（旧版终端）：拒绝握手，响应里给错误码和支持的版本；
//   - 什么都没报：普通的 Realtime 客户端，只用标准事件。
package brainserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/semaphore"

	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/heartbeat"
	"github.com/bukahou/haro/internal/mcpbridge"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Session 是一次对话会话（brain.Session 实现了它）。
type Session interface {
	Send(ctx context.Context, event realtimeproto.ClientEvent) error
	Events() <-chan realtimeproto.ServerEvent // 会话期间不关闭
	Close() error
}

// Terminal 是这条连接那头的情况，建会话时交给会话。
type Terminal struct {
	// DeviceTools 是 haro 终端经 MCP 提供设备能力（拍一帧等）的通道，服务端是 MCP 客户端；
	// nil = 普通 Realtime 客户端，没有这条通道。
	DeviceTools mcp.Transport
}

// SessionFactory 为每条连接新建一个会话。
type SessionFactory func(terminal Terminal) Session

// Options 是连接层的设置。零值可用。
type Options struct {
	Heartbeat   heartbeat.Timing // 零值 = heartbeat.Default
	MaxSessions int              // 同时最多几条对话会话；零值 = realtimeproto.MaxSessions（和 haro-asr 的转写会话同一个数）
}

// haroProtocolPrefix 是 haro 各版本子协议名的共同开头。
const haroProtocolPrefix = "haro.realtime."

// NewHandler 返回挂好路由的 HTTP handler。
func NewHandler(newSession SessionFactory, opts Options, logger *slog.Logger) http.Handler {
	opts.Heartbeat = opts.Heartbeat.OrDefault()
	opts.MaxSessions = cmp.Or(opts.MaxSessions, realtimeproto.MaxSessions)
	sessions := semaphore.NewWeighted(int64(opts.MaxSessions))
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+realtimeproto.ConversationPath, func(w http.ResponseWriter, r *http.Request) {
		// 先限量再做事：满了就回 503，不握手、不建会话（不连 haro-asr）
		if !sessions.TryAcquire(1) {
			logger.Warn("拒绝连接：对话会话数到了上限", "remote", r.RemoteAddr, "limit", opts.MaxSessions)
			writeHTTPError(w, http.StatusServiceUnavailable, realtimeproto.NewHTTPError(http.StatusServiceUnavailable,
				realtimeproto.CodeTooManySessions, fmt.Sprintf("服务端同时最多 %d 条对话会话，现在满了：稍后再连", opts.MaxSessions)))
			return
		}
		defer sessions.Release(1)
		serveConversation(w, r, newSession, opts, logger)
	})
	return mux
}

// writeHTTPError 回一个出错的 HTTP 响应（握手之前）：状态码 + Realtime 形状的错误说明。
func writeHTTPError(w http.ResponseWriter, status int, body realtimeproto.HTTPError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
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
	case slices.Contains(offered, realtimeproto.Protocol):
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
	body.Error.SupportedProtocols = []string{realtimeproto.Protocol}
	writeHTTPError(w, http.StatusBadRequest, body)
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
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{realtimeproto.Protocol}})
	if err != nil {
		logger.Warn("WebSocket 握手失败", "remote", r.RemoteAddr, "err", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(realtimeproto.MaxMessageBytes)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

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

	var terminal Terminal
	var tools *mcpbridge.Transport // haro 终端的设备能力（MCP）；普通客户端没有
	if protocol == realtimeproto.Protocol {
		tools = mcpbridge.NewTransport(func(_ context.Context, message json.RawMessage) error {
			return write(realtimeproto.HaroMCPMessage{Message: message})
		})
		defer tools.Close()
		terminal.DeviceTools = tools
	}
	session := newSession(terminal)
	defer session.Close()
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
		if message, ok := event.(realtimeproto.HaroMCPMessage); ok { // 设备能力：交给 MCP 会话，不给对话
			if tools == nil {
				_ = write(realtimeproto.Error{Code: realtimeproto.CodeUnsupportedFeature,
					Message: "haro.mcp.message: 普通 Realtime 客户端没有设备能力的通道"})
			} else if err := tools.Deliver(ctx, message.Message); err != nil && ctx.Err() == nil {
				logger.Warn("设备能力（MCP）的消息交不出去", "remote", r.RemoteAddr, "err", err)
			}
			continue
		}
		if err := session.Send(ctx, event); err != nil {
			_ = write(realtimeproto.ErrorEventFor(event.Type(), err))
			continue
		}
		// 会话收下了 session.update 才按它声明的格式解之后的音频（被拒的 session.update 整条不生效）
		if update, ok := event.(realtimeproto.SessionUpdate); ok && update.InputFormat.SampleRate > 0 {
			inputFormat = update.InputFormat
		}
	}
}
