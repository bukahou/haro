// Package tools 把内置工具和第三方 MCP 工具服务汇总成服务端能用的 Toolbox（v2.5；v5 改为下面的分工）。
//
// v5 裁定：自研工具（时间、记事本、网络诊断……）一律内置，在服务端进程里直接执行（各类一个 XxxTools 函数，返回 []Builtin）；
// MCP 只用于第三方服务。MCP 服务现在由 haro 作为子进程启动，经 stdio 通信（官方 Go SDK 的 CommandTransport）；
// 远程服务（Streamable HTTP）在 v5 补上。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
)

// Toolbox 实现 brain.Toolbox。
type Toolbox struct {
	specs    []brain.ToolSpec
	servers  map[string]*MCPServer // 工具名 → 所属服务
	builtins map[string]func(context.Context, map[string]any) (string, error)
	all      []*MCPServer
	logger   *slog.Logger
}

// NewToolbox 启动并连接各第三方 MCP 服务，汇总工具清单；任何一个服务起不来就整体失败（并清理已启动的）。
// extra 是自研的内置工具（记事本、网络诊断……）；「现在几点」总是内置。
func NewToolbox(ctx context.Context, servers []ServerSpec, logger *slog.Logger, extra ...Builtin) (*Toolbox, error) {
	box := &Toolbox{servers: map[string]*MCPServer{}, logger: logger,
		builtins: map[string]func(context.Context, map[string]any) (string, error){}}
	box.addBuiltin(brain.ToolSpec{Name: "get_current_time", Description: "获取现在的日期、时间和星期。",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}, currentTime)
	for _, builtin := range extra {
		box.addBuiltin(builtin.Spec, builtin.Run)
	}
	for _, spec := range servers {
		server, err := ConnectMCPServer(ctx, spec, logger)
		if err != nil {
			box.Close()
			return nil, err
		}
		box.all = append(box.all, server)
		for _, tool := range server.tools {
			parameters, err := json.Marshal(tool.InputSchema)
			if err != nil {
				box.Close()
				return nil, fmt.Errorf("%s 的工具 %s 参数格式有误: %w", spec.Name, tool.Name, err)
			}
			box.specs = append(box.specs, brain.ToolSpec{Name: tool.Name, Description: tool.Description,
				Parameters: parameters, RequiresConfirmation: requiresConfirmation(tool.Annotations)})
			box.servers[tool.Name] = server
		}
	}
	return box, nil
}

// requiresConfirmation 按 MCP 规范解读标注：只读的不需要；非只读时 destructiveHint 缺省视为 true（规范的默认值）。
func requiresConfirmation(annotations *mcp.ToolAnnotations) bool {
	if annotations == nil {
		return true
	}
	if annotations.ReadOnlyHint {
		return false
	}
	return annotations.DestructiveHint == nil || *annotations.DestructiveHint
}

func (b *Toolbox) addBuiltin(spec brain.ToolSpec, run func(context.Context, map[string]any) (string, error)) {
	b.specs = append(b.specs, spec)
	b.builtins[spec.Name] = run
}

// Specs 返回全部工具。
func (b *Toolbox) Specs() []brain.ToolSpec { return b.specs }

// Call 执行一次调用。
func (b *Toolbox) Call(ctx context.Context, call brain.ToolCall) (string, error) {
	if run, ok := b.builtins[call.Name]; ok {
		return run(ctx, call.Arguments)
	}
	server, ok := b.servers[call.Name]
	if !ok {
		return "", fmt.Errorf("没有叫 %s 的工具", call.Name)
	}
	return server.Call(ctx, call.Name, call.Arguments)
}

// Close 关闭所有工具服务。
func (b *Toolbox) Close() {
	for _, server := range b.all {
		_ = server.Close()
	}
}

var weekdays = []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

func currentTime(context.Context, map[string]any) (string, error) {
	now := time.Now()
	return fmt.Sprintf(`{"now":"%s","weekday":"%s","timezone":"%s"}`,
		now.Format("2006-01-02 15:04"), weekdays[now.Weekday()], now.Format("MST")), nil
}
