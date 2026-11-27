package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
)

// remoteConnection 是到一个远程 MCP 服务的连接（外部工具服务、联网搜索共用）：用到时才连；连接出了问题
// （不是服务说这次调用有错）就丢掉会话，当次重连再试一次，之后用到时照样重连。
type remoteConnection struct {
	spec   ServerSpec
	verify func(*MCPServer) error // 连上后核对（例如要用的工具都在）；nil = 不核对
	logger *slog.Logger

	mu     sync.Mutex
	server *MCPServer // nil = 还没连上，或者断了
}

func newRemoteConnection(spec ServerSpec, verify func(*MCPServer) error, logger *slog.Logger) *remoteConnection {
	return &remoteConnection{spec: spec, verify: verify, logger: logger}
}

// connected 返回连着的会话；还没连就连上（并按 verify 核对）。
func (r *remoteConnection) connected(ctx context.Context) (*MCPServer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.server != nil {
		return r.server, nil
	}
	server, err := ConnectMCPServer(ctx, r.spec, r.logger)
	if err != nil {
		return nil, err
	}
	if r.verify != nil {
		if err := r.verify(server); err != nil {
			_ = server.Close()
			return nil, err
		}
	}
	r.server = server
	return server, nil
}

// drop 丢掉断了的会话（下次用到时重连）。
func (r *remoteConnection) drop(server *MCPServer) {
	r.mu.Lock()
	if r.server == server {
		r.server = nil
	}
	r.mu.Unlock()
	_ = server.Close()
	r.logger.Warn("和工具服务的会话断了，下次用到时重连", "server", r.spec.Name)
}

// call 调用一个工具（只带上工具清单里声明过的参数）。连接出问题时丢掉会话、重连再试一次。
// 每真的发出一次请求就调一次 sent（err 是那次的结果）；连都没连上时请求没发出去，不调。
func (r *remoteConnection) call(ctx context.Context, name string, arguments map[string]any, sent func(err error)) (string, error) {
	retry := retrypolicy.NewBuilder[string]().WithMaxRetries(1).
		HandleIf(func(_ string, err error) bool { return connectionFailed(ctx, err) }).Build()
	return failsafe.With(retry).WithContext(ctx).Get(func() (string, error) {
		server, err := r.connected(ctx)
		if err != nil {
			return "", fmt.Errorf("连不上 %s: %w", r.spec.Name, err)
		}
		text, err := server.CallKnown(ctx, name, arguments)
		if sent != nil {
			sent(err)
		}
		if connectionFailed(ctx, err) {
			r.drop(server)
		}
		return text, err
	})
}

// connectionFailed 判断这次失败是不是连接出了问题：服务正常回应的工具错误（*ToolError）不是，调用方自己取消、超时的也不算。
func connectionFailed(ctx context.Context, err error) bool {
	var toolErr *ToolError
	return err != nil && !errors.As(err, &toolErr) && ctx.Err() == nil
}

// Close 断开会话（没连着时什么都不做）。
func (r *remoteConnection) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.server == nil {
		return nil
	}
	err := r.server.Close()
	r.server = nil
	return err
}
