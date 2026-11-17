// Package platform 是终端的平台层门面（v4 第 5 步）：本机的麦克风、音箱、摄像头只从这里列出、打开。
// 外面只看到 OpenLocalDevices 和 LocalDevices；用的是哪个操作系统的做法，在编译时自动选定（osdiff），不需要配置。
//
//	platform（门面）
//	  ├ audio   录音放音：miniaudio，Linux / macOS / 树莓派共用
//	  ├ camera  取帧：ffmpeg（输入参数由 osdiff 给）
//	  └ osdiff  各系统不同的部分：认接口类型、列摄像头、摄像头输入参数
//
// LocalDevices 满足服务层（internal/terminal）定义的 DeviceCatalog 与 DeviceOpener；服务层不 import 本包。
package platform

import (
	"context"
	"log/slog"

	"haro/internal/assistant"
	"haro/internal/platform/audio"
	"haro/internal/platform/camera"
	"haro/internal/platform/osdiff"
	"haro/internal/terminal"
)

// LocalDevices 是本机的设备层（整个终端进程一个）。
type LocalDevices struct {
	audio  *audio.Backend
	os     osdiff.Adapter
	logger *slog.Logger
}

// 编译期检查：门面必须满足服务层的两个接口。
var (
	_ terminal.DeviceCatalog = (*LocalDevices)(nil)
	_ terminal.DeviceOpener  = (*LocalDevices)(nil)
)

// OpenLocalDevices 打开本机的设备层。程序退出时调用 Close。
func OpenLocalDevices(logger *slog.Logger) (*LocalDevices, error) {
	backend, err := audio.NewMiniaudioBackend(logger)
	if err != nil {
		return nil, err
	}
	return &LocalDevices{audio: backend, os: osdiff.Current(), logger: logger}, nil
}

// Close 释放音频上下文（所有设备都已关闭之后调用）。
func (d *LocalDevices) Close() { d.audio.Close() }

// Microphones 列出麦克风。
func (d *LocalDevices) Microphones() ([]terminal.Device, error) {
	devices, err := d.audio.Microphones()
	return d.describe(devices), err
}

// Speakers 列出音箱。
func (d *LocalDevices) Speakers() ([]terminal.Device, error) {
	devices, err := d.audio.Speakers()
	return d.describe(devices), err
}

// Cameras 列出摄像头。
func (d *LocalDevices) Cameras(ctx context.Context) ([]terminal.Device, error) {
	return d.os.Cameras(ctx)
}

// OpenMicrophone 打开常驻麦克风流；id 为空 = 系统默认。
func (d *LocalDevices) OpenMicrophone(id string) assistant.Microphone {
	return audio.NewMiniaudioMicrophone(d.audio, id)
}

// OpenRecorder 打开一段一段的录音（登记声纹用）。
func (d *LocalDevices) OpenRecorder(id string) assistant.AudioRecorder {
	return audio.NewMiniaudioRecorder(d.audio, id)
}

// OpenSpeaker 打开音箱；id 为空 = 跟随系统默认。
func (d *LocalDevices) OpenSpeaker(id string) assistant.AudioSink {
	return audio.NewMiniaudioSpeaker(d.audio, id)
}

// OpenCamera 打开摄像头：常驻模式在后台运行 ffmpeg 直到 ctx 取消；按需模式每轮现抓。
// 取帧失败后用 pick（服务层的选择规则）重新找摄像头，再按本系统的做法拼 ffmpeg 输入参数。
func (d *LocalDevices) OpenCamera(ctx context.Context, startup terminal.Choice, pick terminal.CameraPicker,
	options terminal.CameraOptions) terminal.Camera {
	locate := func(ctx context.Context) ([]string, string, bool) {
		choice, ok := pick(ctx)
		if !ok {
			return nil, "", false
		}
		return d.os.CameraInput(choice.ID, options.CaptureSize), choice.Name, true
	}
	if !options.Continuous {
		return camera.NewOnDemandCamera(locate, startup.Name, options.OutputWidth, d.logger)
	}
	continuous := camera.NewContinuousCamera(locate, startup.Name, options.OutputWidth, options.FPS,
		options.MaxAge, d.logger)
	go continuous.Run(ctx)
	return continuous
}

// describe 把 miniaudio 列出的设备转成服务层的描述（接口类型由各系统认）。
func (d *LocalDevices) describe(devices []audio.Device) []terminal.Device {
	described := make([]terminal.Device, len(devices))
	for i, device := range devices {
		described[i] = terminal.Device{ID: device.Key, Name: device.Name, Default: device.Default,
			Bus: d.os.AudioBus(device.Key, device.Name)}
	}
	return described
}
