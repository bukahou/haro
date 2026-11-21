package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
)

// Builtin 往进程内的工具服务上注册一组内置工具（用 AddTool）。
type Builtin func(server *mcp.Server)

// 调用内置工具时放进请求元数据（_meta）的键，键名只在这里定义。过了 MCP 这层，context 里的值带不过去：
// 说话人直接带；「主人听完才算数的事」（brain.Deliveries）是回调，只带这次调用的编号，进程内按编号找回。
const (
	metaSpeaker = "haro/speaker"
	metaCall    = "haro/call"
	metaSpoken  = "haro/spoken" // 结果里：这个错误可以直接念给主人（brain.SpokenError），值是那句话的键和要填的值
)

// AddTool 注册一个内置工具：参数 In、结果 Out 都是结构体，工具定义由它们生成，调用前按定义校验参数
// （mcp.AddTool；字段的 jsonschema 标签是给模型看的说明，带 omitempty 的是可选参数）。
// 工具返回能直接念给主人的错误（brain.SpokenError，例如额度用完）时，结果里带上标记，Toolbox 那边还原成同样的错误。
func AddTool[In, Out any](server *mcp.Server, tool *mcp.Tool, run func(context.Context, In) (Out, error)) {
	claimToolName(server, tool.Name)
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := run(ctx, in)
		if err != nil {
			result, err := failedResult(err)
			return result, out, err
		}
		return nil, out, nil
	})
}

// AddTextTool 注册一个结果是一段文字的内置工具（例如搜索服务整理好的结果）：文字原样交给模型。参数同 AddTool。
func AddTextTool[In any](server *mcp.Server, tool *mcp.Tool, run func(context.Context, In) (string, error)) {
	claimToolName(server, tool.Name)
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		text, err := run(ctx, in)
		if err != nil {
			result, err := failedResult(err)
			return result, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}

// failedResult 是工具出错时交回去的：能念给主人的错误做成带标记的结果（Toolbox 那边还原），其余的交给 SDK 包成工具错误。
func failedResult(err error) (*mcp.CallToolResult, error) {
	var spoken brain.SpokenError
	if errors.As(err, &spoken) {
		key, values := spoken.Phrase()
		result := &mcp.CallToolResult{Meta: mcp.Meta{metaSpoken: spokenMeta{Phrase: key, Values: values}}}
		result.SetError(err)
		return result, nil
	}
	return nil, err
}

// spokenMeta 是结果元数据里「能念给主人的错误」：那句话的键和要填的值。
type spokenMeta struct {
	Phrase string            `json:"phrase"`
	Values map[string]string `json:"values,omitempty"`
}

// registeredNames 记下每个进程内服务上注册过的工具名：同一个服务上同名的工具会被悄悄覆盖，要在启动时报出来。
var registeredNames sync.Map // *mcp.Server → *toolNames

type toolNames struct {
	mu         sync.Mutex
	seen       map[string]bool
	duplicates []string
}

func claimToolName(server *mcp.Server, name string) {
	value, _ := registeredNames.LoadOrStore(server, &toolNames{seen: map[string]bool{}})
	names := value.(*toolNames)
	names.mu.Lock()
	defer names.mu.Unlock()
	if names.seen[name] {
		names.duplicates = append(names.duplicates, name)
	}
	names.seen[name] = true
}

// takeDuplicateNames 取出（并忘掉）这个服务上重名的工具。
func takeDuplicateNames(server *mcp.Server) []string {
	value, ok := registeredNames.LoadAndDelete(server)
	if !ok {
		return nil
	}
	return value.(*toolNames).duplicates
}

// ReadOnly、Changes、Destructive 是工具的标注（MCP 规范）：确认闸只看「会破坏」（brain.ToolSpec.RequiresConfirmation）。
var (
	ReadOnly    = &mcp.ToolAnnotations{ReadOnlyHint: true}
	Changes     = &mcp.ToolAnnotations{DestructiveHint: new(false)} // 会改数据但不会丢东西（记一条、恢复、派调查）
	Destructive = &mcp.ToolAnnotations{DestructiveHint: new(true)}  // 会删掉或覆盖东西：执行前要主人确认
)

// builtinServer 是大脑进程里的 MCP 服务，经内存连接接到 Toolbox 的客户端。
type builtinServer struct {
	*MCPServer
	calls  sync.Map // 调用编号 → 这次调用的 brain.Deliveries
	nextID atomic.Int64
}

// startBuiltinServer 建进程内的 MCP 服务、注册内置工具，用内存连接接上客户端。ctx 是服务端的运行期。
func startBuiltinServer(ctx context.Context, builtins []Builtin, logger *slog.Logger) (*builtinServer, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "haro-builtin", Version: "1"}, nil)
	b := &builtinServer{}
	server.AddReceivingMiddleware(b.restoreContext)
	for _, register := range builtins {
		register(server)
	}
	if duplicates := takeDuplicateNames(server); len(duplicates) > 0 {
		return nil, fmt.Errorf("内置工具重名：%v", duplicates)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		return nil, fmt.Errorf("启动内置工具服务: %w", err)
	}
	connected, err := connectMCP(ctx, "builtin", clientTransport, logger)
	if err != nil {
		return nil, err
	}
	b.MCPServer = connected
	return b, nil
}

// restoreContext 在服务这一侧把元数据还原成工具要的 context：说话人、这一轮的收集处。
func (b *builtinServer) restoreContext(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if call, ok := request.(*mcp.CallToolRequest); ok && call.Params != nil {
			meta := call.Params.GetMeta()
			if speaker, ok := meta[metaSpeaker].(string); ok {
				ctx = brain.WithSpeaker(ctx, speaker)
			}
			if id, ok := meta[metaCall].(string); ok {
				if deliveries, ok := b.calls.Load(id); ok {
					ctx = brain.WithDeliveries(ctx, deliveries.(brain.Deliveries))
				}
			}
		}
		return next(ctx, method, request)
	}
}

// call 调用一个内置工具：把 ctx 里的说话人和收集处经元数据带过去。
func (b *builtinServer) call(ctx context.Context, name string, arguments map[string]any) (string, error) {
	meta := mcp.Meta{metaSpeaker: brain.SpeakerFrom(ctx)}
	if deliveries := brain.DeliveriesFrom(ctx); deliveries != nil {
		id := strconv.FormatInt(b.nextID.Add(1), 10)
		b.calls.Store(id, deliveries)
		defer b.calls.Delete(id)
		meta[metaCall] = id
	}
	return b.callWithMeta(ctx, name, arguments, meta)
}
