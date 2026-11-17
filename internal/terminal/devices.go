package terminal

import (
	"context"
	"time"

	"github.com/bukahou/haro/internal/assistant"
)

// 服务层需要的设备能力（v4 第 5 步「底层分层」）：服务层只定义「需要什么」，平台层（internal/platform）去实现。
// 服务层不认识任何操作系统：设备 ID 对这里不透明，只原样传回平台层；选设备的规则只看 Bus（P-3）。

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

// DeviceCatalog 列出本机设备（看守每 2 秒列一次麦克风和音箱；摄像头只在启动时、以及取帧失败后才列，P-1）。
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
	// OpenCamera 打开启动时选中的摄像头；取帧失败后用 pick 重新找（拔掉再插上、换了设备号都能接上）。
	// 常驻模式在后台一直取帧，直到 ctx 取消。
	OpenCamera(ctx context.Context, startup Choice, pick CameraPicker, options CameraOptions) Camera
}

// Camera 是打开的摄像头：能取帧，也能说出现在能不能用（看守据此告诉服务端，没有摄像头时服务端不给模型看画面的工具）。
type Camera interface {
	assistant.FrameSource
	CurrentName() string // 现在能用的摄像头名字；不可用时为空
}

// CameraPicker 按选择规则找现在要用的摄像头；ok=false 表示现在没有。
type CameraPicker func(ctx context.Context) (choice Choice, ok bool)

// CameraOptions 是取帧方式（来自 HARO_CAMERA_* 配置）。
type CameraOptions struct {
	Continuous  bool          // true = 常驻（随时有最新一帧）；false = 每轮说完时现抓
	CaptureSize string        // 摄像头采集分辨率，例如 1280x720
	OutputWidth int           // 送给模型的宽度，高度按比例
	FPS         int           // 常驻模式的输出帧率
	MaxAge      time.Duration // 常驻模式：超过这么久没有新帧，视为摄像头出问题
}

// Choice 是选中的设备。
type Choice struct {
	ID     string // 交给平台层打开的 ID；空 = 系统默认（音箱跟随切换）
	Name   string // 给人看的名字（日志、告诉服务端）
	Reason string // 为什么选它（日志）
}
