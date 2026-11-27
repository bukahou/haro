package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
)

// 外部的 MCP 工具服务（第三方、AtlHyper 这类）：在配置文件里列出（HARO_MCP_SERVERS_FILE），不改代码就能接上、拿掉。
// 文件格式照 Claude Code 的 .mcp.json（mcpServers → 名字 → type / url / headers），另加两项：
//   - tools：只开放这几个工具（允许名单，必填）；服务那边别的工具不交给模型
//   - confirm：另外要主人确认才能执行的工具。服务自己标了会破坏的（按 MCP 规范，没标注、非只读的都算）本来就要确认，
//     配置只能加、不能免
//
// url、headers 里的 ${名字} 启动时换成环境变量的值：凭证不写进文件。

// RemoteServer 是配置文件里的一个外部 MCP 服务（已校验、环境变量已展开）。
type RemoteServer struct {
	Name    string
	URL     string
	Headers map[string]string
	Tools   []string // 允许名单
	Confirm []string // 另外要主人确认的（都在允许名单里）
}

type remoteServersFile struct {
	MCPServers map[string]remoteServerEntry `json:"mcpServers"`
}

type remoteServerEntry struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Tools   []string          `json:"tools"`
	Confirm []string          `json:"confirm,omitempty"`
}

// remoteTransportHTTP 是唯一支持的传输：Streamable HTTP。服务都单独运行，服务端不在本机拉起程序（stdio）。
const remoteTransportHTTP = "http"

var remoteServerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// LoadRemoteServers 读外部 MCP 服务的配置文件；lookup 查环境变量（传 os.LookupEnv）。认不出的字段、没列允许名单、
// confirm 里有名单外的工具、${名字} 对应的环境变量没设或为空，都报错（格式写错不静默用别的值）。按名字排序。
func LoadRemoteServers(path string, lookup func(string) (string, bool)) ([]RemoteServer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读外部 MCP 服务的配置 %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file remoteServersFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("外部 MCP 服务的配置 %s 格式不对: %w", path, err)
	}
	if len(file.MCPServers) == 0 {
		return nil, fmt.Errorf("外部 MCP 服务的配置 %s 里没有服务（mcpServers 是空的）；不接外部服务就别设这个文件", path)
	}
	var servers []RemoteServer
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(file.MCPServers)) {
		server, err := file.MCPServers[name].resolve(name, lookup)
		if err != nil {
			problems = append(problems, fmt.Errorf("外部 MCP 服务 %s：%w", name, err))
			continue
		}
		servers = append(servers, server)
	}
	if err := errors.Join(problems...); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return servers, nil
}

// resolve 校验一个服务的配置、展开环境变量。
func (entry remoteServerEntry) resolve(name string, lookup func(string) (string, bool)) (RemoteServer, error) {
	if !remoteServerName.MatchString(name) {
		return RemoteServer{}, fmt.Errorf("名字只能用小写字母、数字、- 和 _（最长 32 个字）")
	}
	if entry.Type != remoteTransportHTTP {
		return RemoteServer{}, fmt.Errorf("type 只能是 %q（Streamable HTTP），现在是 %q", remoteTransportHTTP, entry.Type)
	}
	var missing []string
	expand := func(value string) string {
		return os.Expand(value, func(variable string) string {
			found, ok := lookup(variable)
			if !ok || found == "" {
				missing = append(missing, variable)
			}
			return found
		})
	}
	server := RemoteServer{Name: name, URL: expand(entry.URL), Headers: map[string]string{}}
	for header, value := range entry.Headers {
		server.Headers[header] = expand(value)
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return RemoteServer{}, fmt.Errorf("要用的环境变量没设或为空：%s", strings.Join(slices.Compact(missing), "、"))
	}
	if parsed, err := url.Parse(server.URL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return RemoteServer{}, fmt.Errorf("url 要是 http(s) 的地址")
	}
	if len(entry.Tools) == 0 {
		return RemoteServer{}, fmt.Errorf("tools 要列出开放哪些工具（允许名单）")
	}
	for _, tool := range entry.Tools {
		if strings.TrimSpace(tool) == "" {
			return RemoteServer{}, fmt.Errorf("tools 里有空的名字")
		}
	}
	for _, tool := range entry.Confirm {
		if !slices.Contains(entry.Tools, tool) {
			return RemoteServer{}, fmt.Errorf("confirm 里的 %s 不在 tools 里", tool)
		}
	}
	server.Tools, server.Confirm = slices.Clone(entry.Tools), slices.Clone(entry.Confirm)
	return server, nil
}

// 连外部服务的时间：启动时连一次最多等多久；连不上时后台重连的间隔（退避，从短到长）。是变量只为测试能调短。
var (
	remoteConnectTimeout  = 10 * time.Second
	remoteReconnectFirst  = 5 * time.Second
	remoteReconnectLatest = 5 * time.Minute
)

// remoteTools 执行一个外部服务开放的工具（经 remoteConnection：断了自动重连），每次调用写一行日志。
type remoteTools struct {
	name       string
	connection *remoteConnection
	logger     *slog.Logger
}

func (r *remoteTools) call(ctx context.Context, name string, arguments map[string]any) (string, error) {
	start := time.Now()
	text, err := r.connection.call(ctx, name, arguments, nil)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		r.logger.Warn("外部工具调用失败", "server", r.name, "tool", name, "elapsed", elapsed, "err", err)
		return "", err
	}
	r.logger.Info("外部工具调用", "server", r.name, "tool", name, "elapsed", elapsed)
	return text, nil
}

// AttachRemoteServers 接上外部 MCP 服务：各连一次，连上的把允许名单里的工具加进清单（和已有工具重名就报错）；
// 连不上的不影响启动，在后台按退避重连，连上后再加（重名时不加、写日志）。ctx 是服务端的运行期：后台重连随它结束。
func (b *Toolbox) AttachRemoteServers(ctx context.Context, servers []RemoteServer, client *http.Client) error {
	for _, server := range servers {
		remote := &remoteTools{name: server.Name, logger: b.logger,
			connection: newRemoteConnection(ServerSpec{Name: server.Name, URL: server.URL, Headers: server.Headers, Client: client},
				nil, b.logger)}
		b.addCloser(remote.connection.Close)
		connectCtx, cancel := context.WithTimeout(ctx, remoteConnectTimeout)
		connected, err := remote.connection.connected(connectCtx)
		cancel()
		if err != nil {
			b.logger.Warn("外部 MCP 服务连不上：在后台接着连，连上后它的工具才能用", "server", server.Name, "err", err)
			go b.attachLater(ctx, server, remote)
			continue
		}
		if err := b.addRemote(server, connected, remote); err != nil {
			return err
		}
	}
	return nil
}

// attachLater 在后台按退避重连，连上后把工具加进清单。
func (b *Toolbox) attachLater(ctx context.Context, server RemoteServer, remote *remoteTools) {
	retry := retrypolicy.NewBuilder[*MCPServer]().WithMaxRetries(-1).WithBackoff(remoteReconnectFirst, remoteReconnectLatest).
		HandleIf(func(_ *MCPServer, err error) bool { return err != nil && ctx.Err() == nil }).
		OnRetry(func(event failsafe.ExecutionEvent[*MCPServer]) {
			b.logger.Warn("外部 MCP 服务还是连不上", "server", server.Name, "attempt", event.Attempts(), "err", event.LastError())
		}).Build()
	connected, err := failsafe.With(retry).WithContext(ctx).Get(func() (*MCPServer, error) {
		connectCtx, cancel := context.WithTimeout(ctx, remoteConnectTimeout)
		defer cancel()
		return remote.connection.connected(connectCtx)
	})
	if err != nil {
		return // 服务端在关停
	}
	if err := b.addRemote(server, connected, remote); err != nil {
		b.logger.Error("外部 MCP 服务连上了，但工具加不进清单", "server", server.Name, "err", err)
	}
}

// addRemote 把一个外部服务允许名单里的工具加进清单；名单里有、服务那边没有的写日志（服务改版了？），不算错。
func (b *Toolbox) addRemote(server RemoteServer, connected *MCPServer, remote *remoteTools) error {
	var offered []string
	var absent []string
	for _, name := range server.Tools {
		if connected.Tool(name) == nil {
			absent = append(absent, name)
			continue
		}
		offered = append(offered, name)
	}
	if len(absent) > 0 {
		b.logger.Warn("允许名单里的工具外部服务那边没有", "server", server.Name, "absent", absent)
	}
	if err := b.add(connected, remote, offered, server.Confirm); err != nil {
		return err
	}
	b.logger.Info("外部 MCP 服务的工具已加进清单", "server", server.Name, "tools", offered, "hidden", len(connected.tools)-len(offered))
	return nil
}
