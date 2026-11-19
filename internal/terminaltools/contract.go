// Package terminaltools 是终端经 MCP 提供给 haro-server 的设备能力（现在只有「拍一帧」）。
// 终端是 MCP 服务端（Server），haro-server 是客户端（CameraClient），跑在对话连接上（mcpbridge）。
// 工具的名字、参数、结果、错误码只在这个文件里定义，两端共用。
package terminaltools

import "time"

// CaptureFrameTool 是「拍一帧」工具的名字。
const CaptureFrameTool = "capture_frame"

// CaptureFrameInput 是「拍一帧」的参数。
type CaptureFrameInput struct {
	MaxWidth int `json:"max_width,omitempty" jsonschema:"画面最大宽度（像素）；不给 = 终端默认的宽度"`
}

// CaptureFrameOutput 是「拍一帧」的结构化结果；画面本身在结果的图片内容里（JPEG）。
type CaptureFrameOutput struct {
	TakenAt string       `json:"taken_at,omitempty" jsonschema:"拍摄时间（RFC 3339）"`
	Width   int          `json:"width,omitempty"`
	Height  int          `json:"height,omitempty"`
	Device  string       `json:"device,omitempty" jsonschema:"摄像头的名字"`
	Error   CaptureError `json:"error,omitempty" jsonschema:"拍失败时的错误码：no_camera、camera_busy、timeout、capture_failed"`
}

// CaptureError 是拍失败的错误码：服务端按它决定怎么告诉模型，不看说明文字。
type CaptureError string

const (
	ErrorNoCamera      CaptureError = "no_camera"
	ErrorCameraBusy    CaptureError = "camera_busy"
	ErrorTimeout       CaptureError = "timeout"
	ErrorCaptureFailed CaptureError = "capture_failed"
)

// MaxFrameBytes 是一帧画面的上限：超过就缩小再回（还要经 base64 装进一条 WebSocket 消息）。
const MaxFrameBytes = 1 << 20

// CaptureTimeout 是服务端等一帧的上限（含终端打开摄像头、预热）：模型在等，到点就当这次没拍成。
const CaptureTimeout = 3 * time.Second
