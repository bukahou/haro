package terminaltools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/image/draw"

	"github.com/bukahou/haro/internal/terminal"
)

// Server 是终端的 MCP 服务端，一个终端进程一个；每条对话连接 Serve 一次。
// 工具清单随设备变化：摄像头能用时有「拍一帧」，拔掉就去掉；清单变了 SDK 自动通知服务端。
type Server struct {
	mcp    *mcp.Server
	camera terminal.Camera // nil = 这台终端不用摄像头
	logger *slog.Logger

	mu            sync.Mutex
	cameraOffered bool
}

var _ terminal.DeviceTools = (*Server)(nil)

// NewServer 创建服务端；camera 为 nil 时永远不提供拍照。
func NewServer(camera terminal.Camera, version string, logger *slog.Logger) *Server {
	// HasTools：一开始没有摄像头时也声明「有工具、清单会变」，之后插上摄像头，服务端才收得到清单变化的通知
	server := mcp.NewServer(&mcp.Implementation{Name: "haro-terminal", Version: version}, &mcp.ServerOptions{HasTools: true})
	return &Server{mcp: server, camera: camera, logger: logger}
}

// SetCameraAvailable 按摄像头现在能不能用，增删「拍一帧」。
func (s *Server) SetCameraAvailable(available bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	available = available && s.camera != nil
	if available == s.cameraOffered {
		return
	}
	s.cameraOffered = available
	if available {
		mcp.AddTool(s.mcp, &mcp.Tool{Name: CaptureFrameTool, Description: "用摄像头拍一帧，回 JPEG 画面和拍摄信息。",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, s.captureFrame)
		return
	}
	s.mcp.RemoveTools(CaptureFrameTool)
}

// Serve 在一条连接上跑一个 MCP 会话，直到连接断开（transport 关闭）或 ctx 结束。
func (s *Server) Serve(ctx context.Context, transport mcp.Transport) error {
	session, err := s.mcp.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("MCP 会话: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	return session.Wait()
}

func (s *Server) captureFrame(ctx context.Context, _ *mcp.CallToolRequest, input CaptureFrameInput) (*mcp.CallToolResult,
	CaptureFrameOutput, error) {
	started := time.Now()
	frame, err := s.camera.Capture(ctx, input.MaxWidth)
	if err != nil {
		code := captureErrorCode(err)
		s.logger.Warn("📷 拍照失败", "code", code, "err", err)
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}},
			CaptureFrameOutput{Error: code}, nil
	}
	data, width, height, err := fitFrame(frame.JPEG, frame.Width, frame.Height, MaxFrameBytes)
	if err != nil {
		s.logger.Warn("📷 画面缩不到上限以内", "err", err)
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}},
			CaptureFrameOutput{Error: ErrorCaptureFailed}, nil
	}
	s.logger.Info("📷 拍了一帧", "device", frame.Device, "size", fmt.Sprintf("%dx%d", width, height), "bytes", len(data),
		"elapsed", time.Since(started).Round(time.Millisecond))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: "image/jpeg"}}},
		CaptureFrameOutput{TakenAt: frame.TakenAt.Format(time.RFC3339Nano), Width: width, Height: height, Device: frame.Device}, nil
}

// captureErrorCode 把拍照的错误换成错误码。
func captureErrorCode(err error) CaptureError {
	switch {
	case errors.Is(err, terminal.ErrNoCamera):
		return ErrorNoCamera
	case errors.Is(err, terminal.ErrCameraBusy):
		return ErrorCameraBusy
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorTimeout
	default:
		return ErrorCaptureFailed
	}
}

// fitFrame 让画面不超过 limit 字节：超了就按比例缩小重新编码，直到放得下。
func fitFrame(data []byte, width, height, limit int) ([]byte, int, int, error) {
	if len(data) <= limit {
		return data, width, height, nil
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("解码画面: %w", err)
	}
	for scale := 0.7; scale > 0.05; scale *= 0.7 {
		bounds := img.Bounds()
		w, h := max(int(float64(bounds.Dx())*scale), 1), max(int(float64(bounds.Dy())*scale), 1)
		scaled := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), img, bounds, draw.Src, nil)
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, scaled, &jpeg.Options{Quality: terminal.FrameJPEGQuality}); err != nil {
			return nil, 0, 0, err
		}
		if encoded.Len() <= limit {
			return encoded.Bytes(), w, h, nil
		}
	}
	return nil, 0, 0, fmt.Errorf("画面缩到很小仍超过 %d 字节", limit)
}
