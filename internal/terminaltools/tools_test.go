package terminaltools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/mcpbridge"
	"github.com/bukahou/haro/internal/terminal"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeCamera 是终端的假摄像头：按设好的画面或错误回应。
type fakeCamera struct {
	mu       sync.Mutex
	frame    terminal.Frame
	err      error
	maxWidth int
}

func (c *fakeCamera) Capture(_ context.Context, maxWidth int) (terminal.Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxWidth = maxWidth
	return c.frame, c.err
}

func (c *fakeCamera) CurrentName() string { return "EMEET" }

func (c *fakeCamera) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

// connect 把终端的服务端和服务端的客户端经桥接层背靠背接起来（模拟一条对话连接）。
func connect(t *testing.T, server *Server) *CameraClient {
	t.Helper()
	var terminalSide, serverSide *mcpbridge.Transport
	terminalSide = mcpbridge.NewTransport(func(ctx context.Context, m json.RawMessage) error { return serverSide.Deliver(ctx, m) })
	serverSide = mcpbridge.NewTransport(func(ctx context.Context, m json.RawMessage) error { return terminalSide.Deliver(ctx, m) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		terminalSide.Close()
		serverSide.Close()
	})
	go func() { _ = server.Serve(ctx, terminalSide) }()
	return ConnectCamera(serverSide, "test", quiet)
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 摄像头能用时服务端看得到「拍一帧」，拍到的画面原样送到；拔掉后清单里去掉，插回再加上（终端主动通知）。
func TestCameraOverMCP(t *testing.T) {
	camera := &fakeCamera{frame: terminal.Frame{JPEG: []byte("jpeg"), TakenAt: time.Now(), Width: 640, Height: 360, Device: "EMEET"}}
	server := NewServer(camera, "test", quiet)
	client := connect(t, server)
	time.Sleep(50 * time.Millisecond)
	if client.Available() {
		t.Fatal("终端还没说摄像头能用，服务端不该以为能拍")
	}

	server.SetCameraAvailable(true)
	waitFor(t, "服务端看到「拍一帧」", client.Available)
	frame, err := client.Capture(context.Background())
	if err != nil || string(frame) != "jpeg" {
		t.Fatalf("拍一帧：%q %v", frame, err)
	}
	if camera.maxWidth != 0 {
		t.Errorf("服务端没指定宽度时应用终端默认（0），收到 %d", camera.maxWidth)
	}

	server.SetCameraAvailable(false)
	waitFor(t, "拔掉后服务端看不到「拍一帧」", func() bool { return !client.Available() })
	server.SetCameraAvailable(true)
	waitFor(t, "插回后又看得到", client.Available)
}

// 拍失败时：服务端拿到带错误码的 *CaptureFailure（按码判断，不看文字），不是空图。
func TestCaptureFailuresCarryCodes(t *testing.T) {
	camera := &fakeCamera{}
	server := NewServer(camera, "test", quiet)
	server.SetCameraAvailable(true)
	client := connect(t, server)
	waitFor(t, "连上", client.Available)
	for _, c := range []struct {
		err  error
		code CaptureError
	}{
		{fmt.Errorf("找不到: %w", terminal.ErrNoCamera), ErrorNoCamera},
		{fmt.Errorf("打开: %w", terminal.ErrCameraBusy), ErrorCameraBusy},
		{fmt.Errorf("等画面: %w", context.DeadlineExceeded), ErrorTimeout},
		{errors.New("驱动出错"), ErrorCaptureFailed},
	} {
		camera.fail(c.err)
		_, err := client.Capture(context.Background())
		failure, ok := errors.AsType[*CaptureFailure](err)
		if !ok || failure.Code != c.code {
			t.Errorf("%v：应得到错误码 %s，得到 %v", c.err, c.code, err)
		}
	}
}

// 取终端的工具清单偶尔失败（例如一次超时）：隔一会儿再取，摄像头照样用得上，不用等整条连接重连。
func TestCameraClientRetriesToolList(t *testing.T) {
	saved := defaultListRetry
	defaultListRetry = listRetryTiming{delay: 5 * time.Millisecond, maxDelay: 10 * time.Millisecond, retries: 4}
	t.Cleanup(func() { defaultListRetry = saved })
	server := NewServer(&fakeCamera{}, "test", quiet)
	server.SetCameraAvailable(true)
	var lists atomic.Int32
	server.mcp.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" && lists.Add(1) <= 2 {
				return nil, errors.New("偶发的失败")
			}
			return next(ctx, method, request)
		}
	})
	client := connect(t, server)
	waitFor(t, "取清单失败两次后再取到、看得到「拍一帧」", client.Available)
}

// 没有摄像头的终端（camera 为 nil）永远不提供「拍一帧」。
func TestServerWithoutCameraNeverOffersCapture(t *testing.T) {
	server := NewServer(nil, "test", quiet)
	server.SetCameraAvailable(true)
	client := connect(t, server)
	time.Sleep(100 * time.Millisecond)
	if client.Available() {
		t.Error("没有摄像头的终端不该提供「拍一帧」")
	}
}

// 画面超过上限：按比例缩小到放得下；没超就原样。
func TestFitFrame(t *testing.T) {
	noisy := image.NewRGBA(image.Rect(0, 0, 640, 480))
	random := rand.New(rand.NewPCG(1, 2))
	for i := range noisy.Pix {
		noisy.Pix[i] = uint8(random.IntN(256)) // 噪声压不小，JPEG 会很大
	}
	noisy.Set(0, 0, color.White)
	var encoded bytes.Buffer
	_ = jpeg.Encode(&encoded, noisy, &jpeg.Options{Quality: 95})
	limit := encoded.Len() / 3
	data, width, height, err := fitFrame(encoded.Bytes(), 640, 480, limit)
	if err != nil || len(data) > limit || width >= 640 || height >= 480 {
		t.Fatalf("应缩到 %d 字节以内：%d 字节，%dx%d，%v", limit, len(data), width, height, err)
	}
	if config, err := jpeg.DecodeConfig(bytes.NewReader(data)); err != nil || config.Width != width || config.Height != height {
		t.Errorf("缩小后的画面和报的宽高对不上：%+v %v", config, err)
	}
	same, w, h, err := fitFrame(encoded.Bytes(), 640, 480, encoded.Len())
	if err != nil || !bytes.Equal(same, encoded.Bytes()) || w != 640 || h != 480 {
		t.Error("没超上限时应原样返回")
	}
}
