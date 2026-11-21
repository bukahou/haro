// Package tools 把大脑能用的工具汇总成一个 Toolbox：内置工具注册在进程内的 MCP 服务上（经内存连接调用），
// 第三方的是远程 MCP 服务；对模型只有一份工具清单。每个内置工具的参数、结果都是结构体（AddTool），
// 定义由结构体生成、调用前校验；每个工具标注只读 / 会改数据 / 会破坏，确认闸只看「会破坏」。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
)

// Toolbox 实现 brain.Toolbox。
type Toolbox struct {
	specs   []brain.ToolSpec
	callers map[string]toolCaller // 工具名 → 由谁执行（进程内的服务、某个远程服务）
	servers []*MCPServer          // 关停时要断开的
}

// toolCaller 执行一个工具调用（进程内的服务要额外经元数据带上说话人等）。
type toolCaller interface {
	call(ctx context.Context, name string, arguments map[string]any) (string, error)
}

// NewToolbox 建进程内的工具服务（内置工具），汇总工具清单。起不来、或者工具重名，就失败。ctx 是服务端的运行期。
// 远程的第三方 MCP 服务现在没有接：要接时在这里按 ServerSpec 连上并 add（重名检查照样生效）。
func NewToolbox(ctx context.Context, logger *slog.Logger, builtins ...Builtin) (*Toolbox, error) {
	box := &Toolbox{callers: map[string]toolCaller{}}
	builtin, err := startBuiltinServer(ctx, builtins, logger)
	if err != nil {
		return nil, err
	}
	box.servers = append(box.servers, builtin.MCPServer)
	if err := box.add(builtin.MCPServer, builtin); err != nil {
		box.Close()
		return nil, err
	}
	return box, nil
}

// add 把一个服务的工具加进清单；和已有的重名就报错（模型看到两个同名工具，调用哪个说不清）。
func (b *Toolbox) add(server *MCPServer, caller toolCaller) error {
	for _, tool := range server.tools {
		if _, taken := b.callers[tool.Name]; taken {
			return fmt.Errorf("工具重名：%s（%s 里又有一个）", tool.Name, server.name)
		}
		parameters, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return fmt.Errorf("%s 的工具 %s 参数格式有误: %w", server.name, tool.Name, err)
		}
		b.specs = append(b.specs, brain.ToolSpec{Name: tool.Name, Description: tool.Description,
			Parameters: parameters, RequiresConfirmation: requiresConfirmation(tool.Annotations)})
		b.callers[tool.Name] = caller
	}
	return nil
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

// Specs 返回全部工具。
func (b *Toolbox) Specs() []brain.ToolSpec { return b.specs }

// Call 执行一次调用。
func (b *Toolbox) Call(ctx context.Context, call brain.ToolCall) (string, error) {
	caller, ok := b.callers[call.Name]
	if !ok {
		return "", fmt.Errorf("没有叫 %s 的工具", call.Name)
	}
	return caller.call(ctx, call.Name, call.Arguments)
}

// Close 断开所有工具服务。
func (b *Toolbox) Close() {
	for _, server := range b.servers {
		_ = server.Close()
	}
}
