// Package tools 把大脑能用的工具汇总成一个 Toolbox：内置工具注册在进程内的 MCP 服务上（经内存连接调用），
// 外部的是配置文件里列出的远程 MCP 服务（见 remote_servers.go）；对模型只有一份工具清单。每个内置工具的参数、结果都是结构体（AddTool），
// 定义由结构体生成、调用前校验；每个工具标注只读 / 会改数据 / 会破坏，确认闸只看「会破坏」：
// 会破坏的工具执行时还要带着主人的确认（brain.Confirmation），工具箱自己核对。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
)

// Toolbox 实现 brain.Toolbox。
type Toolbox struct {
	logger *slog.Logger

	// 外部服务在后台连上时才加进来：清单在运行中会变，读写都要拿锁
	mu      sync.RWMutex
	specs   []brain.ToolSpec
	callers map[string]toolCaller // 工具名 → 由谁执行（进程内的服务、某个远程服务）
	// confirmed：会破坏的工具（RequiresConfirmation）。执行方自己守门：调用时见不到主人对它的确认
	// （brain.RequireConfirmation）就拒绝，不假设上游已经问过主人
	confirmed map[string]bool
	// schemas：每个工具的参数定义（解析好的）。执行前按它校验，不对就交回 brain.ErrInvalidArguments（叫模型改参数重调），
	// 不交给工具服务：服务那边的校验只给一段文字，分不出是参数不对还是服务出了问题
	schemas map[string]*jsonschema.Resolved
	closers []func() error // 关停时要断开的
}

// toolCaller 执行一个工具调用（进程内的服务要额外经元数据带上说话人等）。
type toolCaller interface {
	call(ctx context.Context, name string, arguments map[string]any) (string, error)
}

// NewToolbox 建进程内的工具服务（内置工具），汇总工具清单。起不来、或者工具重名，就失败。ctx 是服务端的运行期。
// 外部的 MCP 服务之后用 AttachRemoteServers 接上（重名检查照样生效）。
func NewToolbox(ctx context.Context, logger *slog.Logger, builtins ...Builtin) (*Toolbox, error) {
	box := &Toolbox{logger: logger, callers: map[string]toolCaller{}, confirmed: map[string]bool{},
		schemas: map[string]*jsonschema.Resolved{}}
	builtin, err := startBuiltinServer(ctx, builtins, logger)
	if err != nil {
		return nil, err
	}
	box.addCloser(builtin.MCPServer.Close)
	if err := box.add(builtin.MCPServer, builtin, nil, nil); err != nil {
		box.Close()
		return nil, err
	}
	return box, nil
}

// addCloser 登记关停时要断开的连接。
func (b *Toolbox) addCloser(closer func() error) {
	b.mu.Lock()
	b.closers = append(b.closers, closer)
	b.mu.Unlock()
}

// add 把一个服务的工具加进清单：only 不为 nil 时只加这几个（外部服务的允许名单）；confirm 里的另外要主人确认
// （服务自己标了会破坏的本来就要）。和已有的重名就报错、一个都不加（模型看到两个同名工具，调用哪个说不清）。
func (b *Toolbox) add(server *MCPServer, caller toolCaller, only, confirm []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var added []*mcp.Tool
	for _, tool := range server.tools {
		if only != nil && !slices.Contains(only, tool.Name) {
			continue
		}
		if _, taken := b.callers[tool.Name]; taken {
			return fmt.Errorf("工具重名：%s（%s 里又有一个）", tool.Name, server.name)
		}
		added = append(added, tool)
	}
	specs, schemas := make([]brain.ToolSpec, 0, len(added)), make(map[string]*jsonschema.Resolved, len(added))
	for _, tool := range added {
		parameters, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return fmt.Errorf("%s 的工具 %s 参数格式有误: %w", server.name, tool.Name, err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(parameters, &schema); err != nil {
			return fmt.Errorf("%s 的工具 %s 参数定义读不了: %w", server.name, tool.Name, err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return fmt.Errorf("%s 的工具 %s 参数定义解析不了: %w", server.name, tool.Name, err)
		}
		schemas[tool.Name] = resolved
		specs = append(specs, brain.ToolSpec{Name: tool.Name, Description: tool.Description, Parameters: parameters,
			RequiresConfirmation: requiresConfirmation(tool.Annotations) || slices.Contains(confirm, tool.Name)})
	}
	for _, spec := range specs { // 全部校验过才加：不留下加了一半的服务
		b.specs = append(b.specs, spec)
		b.schemas[spec.Name] = schemas[spec.Name]
		b.callers[spec.Name] = caller
		b.confirmed[spec.Name] = spec.RequiresConfirmation
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

// Specs 返回全部工具（外部服务在后台连上后会变多）。
func (b *Toolbox) Specs() []brain.ToolSpec {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return slices.Clone(b.specs)
}

// Call 执行一次调用。参数先按工具的定义校验，不对就交回 brain.ErrInvalidArguments、不执行。
// 会破坏的工具要 ctx 里带着主人对它的确认（brain.WithConfirmation），没有就不执行、交回
// brain.ErrNotConfirmed：不管调用来自哪个入口（会话、以后的手机入口、后台代理），都要先问过主人。
func (b *Toolbox) Call(ctx context.Context, call brain.ToolCall) (string, error) {
	b.mu.RLock()
	caller, ok := b.callers[call.Name]
	schema, confirmed := b.schemas[call.Name], b.confirmed[call.Name]
	b.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("没有叫 %s 的工具", call.Name)
	}
	if err := checkArguments(schema, call); err != nil {
		return "", err
	}
	if confirmed {
		if _, err := brain.RequireConfirmation(ctx, call.Name, ""); err != nil {
			return "", err
		}
	}
	return caller.call(ctx, call.Name, call.Arguments)
}

// checkArguments 按工具的参数定义校验这次调用的参数（照 JSON 的样子校验：模型给的参数本来就是 JSON 解出来的）。
func checkArguments(schema *jsonschema.Resolved, call brain.ToolCall) error {
	arguments := call.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		return brain.InvalidArguments(fmt.Errorf("%s 的参数编不成 JSON: %w", call.Name, err))
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return brain.InvalidArguments(fmt.Errorf("%s 的参数不是 JSON: %w", call.Name, err))
	}
	if err := schema.Validate(value); err != nil {
		return brain.InvalidArguments(fmt.Errorf("参数不符合 %s 的定义：%w", call.Name, err))
	}
	return nil
}

// Close 断开所有工具服务。
func (b *Toolbox) Close() {
	b.mu.Lock()
	closers := b.closers
	b.closers = nil
	b.mu.Unlock()
	for _, closer := range closers {
		_ = closer()
	}
}
