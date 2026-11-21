package terminaltools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CaptureFailure 是终端回报的拍照失败：Code 供程序判断；Error() 是交给模型、能转述给主人的说明。
type CaptureFailure struct {
	Code   CaptureError
	Detail string // 终端给的原因（日志用）
}

func (f *CaptureFailure) Error() string {
	switch f.Code {
	case ErrorNoCamera:
		return "摄像头现在不在（可能被拔掉了），看不到"
	case ErrorCameraBusy:
		return "摄像头被别的程序占着，现在看不了"
	case ErrorTimeout:
		return "摄像头没有及时出画面"
	default:
		return "没拍成"
	}
}

// listTimeout：取一次终端的工具清单最多等这么久。
const listTimeout = 5 * time.Second

// listRetryTiming 是取工具清单失败后怎么再试：从 delay 起翻倍、最长 maxDelay，最多再试 retries 次（会话结束了就不再试）。
type listRetryTiming struct {
	delay, maxDelay time.Duration
	retries         int
}

// defaultListRetry：一次偶发的超时不该让这条连接剩下的时间里摄像头都用不了。定值，没有单独实测。
var defaultListRetry = listRetryTiming{delay: time.Second, maxDelay: 8 * time.Second, retries: 4}

// CameraClient 是 haro-server 这边经 MCP 用终端摄像头的客户端（一条对话连接一个）。
// 初始化、取工具清单在后台完成；连上之前、以及终端没有「拍一帧」时 Available 为 false。
type CameraClient struct {
	logger *slog.Logger
	alive  context.Context // MCP 会话结束（或连不上）时取消：不再取清单
	retry  listRetryTiming

	mu        sync.Mutex
	session   *mcp.ClientSession
	available bool
}

// ConnectCamera 在 transport 上连终端的 MCP 服务端（后台进行，不阻塞）。连接结束（transport 关闭）时随之结束。
func ConnectCamera(transport mcp.Transport, version string, logger *slog.Logger) *CameraClient {
	alive, end := context.WithCancel(context.Background())
	camera := &CameraClient{logger: logger, alive: alive, retry: defaultListRetry}
	client := mcp.NewClient(&mcp.Implementation{Name: "haro-server", Version: version}, &mcp.ClientOptions{
		// 在通知处理里不能等对端的回应（那条回应要由同一个读循环收）：另起 goroutine 重新取清单
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { go camera.refresh() },
	})
	go func() {
		session, err := client.Connect(alive, transport, nil)
		if err != nil {
			end()
			logger.Warn("连不上终端的设备能力（MCP）", "err", err)
			return
		}
		camera.mu.Lock()
		camera.session = session
		camera.mu.Unlock()
		go func() { // 会话结束：还在等着重试的取清单都停下
			_ = session.Wait()
			end()
		}()
		camera.refresh()
	}()
	return camera
}

// refresh 重新取终端的工具清单，看有没有「拍一帧」；取不到就按 listRetry 隔一会儿再试。
func (c *CameraClient) refresh() {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return
	}
	policy := retrypolicy.NewBuilder[*mcp.ListToolsResult]().
		WithBackoff(c.retry.delay, c.retry.maxDelay).
		WithMaxRetries(c.retry.retries).
		OnRetryScheduled(func(event failsafe.ExecutionScheduledEvent[*mcp.ListToolsResult]) {
			c.logger.Warn("取终端的工具清单失败：稍后再试", "err", event.LastError(), "retry_in", event.Delay)
		}).
		Build()
	tools, err := failsafe.With(policy).WithContext(c.alive).Get(func() (*mcp.ListToolsResult, error) {
		ctx, cancel := context.WithTimeout(c.alive, listTimeout)
		defer cancel()
		return session.ListTools(ctx, nil)
	})
	if err != nil {
		if c.alive.Err() == nil { // 会话结束了的不用说
			c.logger.Warn("取终端的工具清单失败：不再试（终端的清单再变时会重新取）", "err", err)
		}
		return
	}
	available := slices.ContainsFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == CaptureFrameTool })
	c.mu.Lock()
	changed := available != c.available
	c.available = available
	c.mu.Unlock()
	if changed {
		c.logger.Info("终端的摄像头", "available", available)
	}
}

// Available 是终端现在能不能拍（工具清单里有没有「拍一帧」）。
func (c *CameraClient) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.available
}

// Capture 让终端拍一帧，返回 JPEG。终端回报失败时返回 *CaptureFailure。
func (c *CameraClient) Capture(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return nil, &CaptureFailure{Code: ErrorNoCamera, Detail: "还没和终端连上"}
	}
	ctx, cancel := context.WithTimeout(ctx, CaptureTimeout)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: CaptureFrameTool, Arguments: CaptureFrameInput{}})
	if err != nil {
		if ctx.Err() != nil {
			return nil, &CaptureFailure{Code: ErrorTimeout, Detail: err.Error()}
		}
		return nil, fmt.Errorf("调用终端的拍照: %w", err)
	}
	if result.IsError {
		failure := &CaptureFailure{Code: ErrorCaptureFailed}
		var output CaptureFrameOutput
		if data, err := json.Marshal(result.StructuredContent); err == nil && json.Unmarshal(data, &output) == nil && output.Error != "" {
			failure.Code = output.Error
		}
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				failure.Detail = text.Text
			}
		}
		return nil, failure
	}
	for _, content := range result.Content {
		if image, ok := content.(*mcp.ImageContent); ok && len(image.Data) > 0 {
			return image.Data, nil
		}
	}
	return nil, errors.New("终端回的结果里没有画面")
}
