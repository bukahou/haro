package terminal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/assistant"
)

// 服务层需要的设备能力：服务层只定义「需要什么」，平台层（internal/platform）去实现。
// 服务层不认识任何操作系统：设备 ID 对这里不透明，只原样传回平台层；选设备的规则只看 Bus。

// Bus 是设备的接口类型。
type Bus int

const (
	BusUnknown   Bus = iota
	BusBuiltin       // 内置（笔记本麦克风、Mac 扬声器）
	BusUSB           // USB
	BusBluetooth     // 蓝牙：当麦克风用时耳机会切到通话模式、音质下降，有别的麦克风就不用它
)

func (b Bus) String() string {
	switch b {
	case BusBuiltin:
		return "内置"
	case BusUSB:
		return "USB"
	case BusBluetooth:
		return "蓝牙"
	default:
		return "未知"
	}
}

// Device 是平台层报上来的一个设备（与系统无关的描述）。
type Device struct {
	ID      string // 系统里的设备标识，只原样传回平台层（Linux：PulseAudio 节点名 / /dev/videoN；macOS：CoreAudio UID / 摄像头序号）
	Name    string // 给人看的名字
	Default bool   // 系统默认设备
	Bus     Bus
}

// DeviceCatalog 列出本机设备（看守每 2 秒列一次麦克风和音箱；摄像头在启动时和每次拍照前列）。
type DeviceCatalog interface {
	Microphones() ([]Device, error)
	Speakers() ([]Device, error)
	Cameras(ctx context.Context) ([]Device, error)
}

// DeviceOpener 按 ID 打开设备，返回 assistant 用的接口。
type DeviceOpener interface {
	OpenMicrophone(id string) assistant.Microphone
	OpenRecorder(id string) assistant.AudioRecorder // 登记声纹用
	OpenSpeaker(id string) assistant.AudioSink      // id 为空 = 跟随系统默认
	// OpenCamera 准备好摄像头（这时并不打开它）；每次拍之前都用 pick 重新找（拔掉再插上、换了设备号都能接上）。
	// 要的拍照方式这个系统上不支持时返回错误。
	OpenCamera(startup Choice, pick CameraPicker, options CameraOptions) (Camera, error)
}

// Camera 是终端的摄像头：要看的时候才打开、拍一帧、马上关上（灯只在拍的时候亮）。
// 能不能用由 CurrentName 告诉看守，看守再告诉服务端（没有摄像头时服务端不给模型看画面的工具）。
type Camera interface {
	// Capture 拍一帧：画面比 maxWidth 宽就缩小（0 = 用 CameraOptions.OutputWidth），更窄就原样。
	Capture(ctx context.Context, maxWidth int) (Frame, error)
	CurrentName() string // 现在能用的摄像头名字；不可用时为空
}

// 拍照失败的几种情况（拍照方按它们决定怎么告诉服务端；其他错误都算「拍照失败」）。
var (
	ErrNoCamera   = errors.New("现在没有摄像头")
	ErrCameraBusy = errors.New("摄像头被别的程序占用")
)

// Frame 是拍到的一帧。
type Frame struct {
	JPEG          []byte
	TakenAt       time.Time
	Width, Height int
	Device        string // 摄像头的名字
}

// DeviceTools 是终端经 MCP 提供给服务端的设备能力（terminaltools.Server 满足它）。
type DeviceTools interface {
	// Serve 在一条连接的 MCP 通道上跑一个会话，直到连接断开或 ctx 结束。
	Serve(ctx context.Context, transport mcp.Transport) error
	// SetCameraAvailable 按摄像头现在能不能用增删「拍一帧」（清单变了会通知服务端）。
	SetCameraAvailable(available bool)
}

// CameraPicker 按选择规则找现在要用的摄像头；ok=false 表示现在没有。
type CameraPicker func(ctx context.Context) (choice Choice, ok bool)

// CameraBackend 是拍照用哪套底层。
type CameraBackend int

const (
	CameraAuto         CameraBackend = iota // 按系统选：支持 mediadevices 的系统用它，其余用 ffmpeg
	CameraMediaDevices                      // pion/mediadevices：预热到曝光稳定再取帧
	CameraFFmpeg                            // ffmpeg 抓一帧（备用；不预热）
)

// CameraOptions 是拍照的设置（来自 HARO_CAMERA_* 配置）。
type CameraOptions struct {
	Backend     CameraBackend
	CaptureSize FrameSize // 想向摄像头要的分辨率（摄像头没有时改要宽度够的里面最小的）
	OutputWidth int       // 默认的最大宽度：Capture 的 maxWidth 为 0 时用它
}

// FrameSize 是画面的宽高（像素）。
type FrameSize struct{ Width, Height int }

func (s FrameSize) String() string { return fmt.Sprintf("%dx%d", s.Width, s.Height) }

// Choice 是选中的设备。
type Choice struct {
	ID     string // 交给平台层打开的 ID；空 = 系统默认（音箱跟随切换）
	Name   string // 给人看的名字（日志、告诉服务端）
	Reason string // 为什么选它（日志）
}
