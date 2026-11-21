// Package osdiff 是各操作系统做法不同的那部分（v4 第 5 步）。录音放音三个系统共用 miniaudio，不在这里；
// 这里只有三件事：认出音频设备的接口类型和是不是回录源、列摄像头、向摄像头要什么格式和分辨率（给 ffmpeg 的输入参数）。
//
// 每个系统一份实现（adapter_linux.go / adapter_darwin.go / adapter_other.go），编译时按 GOOS 自动选中一份，
// 由 Current() 返回。每份都有 `var _ Adapter = …` 编译期检查：少实现一个方法，那个系统的版本就编译不过。
//
// 本包是纯 Go（不含 cgo）：开发机上能用 `make check-darwin`（GOOS=darwin go vet）检查 Mac 版是否满足契约。
// 规则函数（*_rules.go）不带构建标签，各系统的规则在任何机器上都能跑单元测试；要调系统接口的部分在 *_linux.go 等文件里。
package osdiff

import (
	"context"

	"github.com/bukahou/haro/internal/terminal"
)

// Adapter 是每个操作系统各写一份的部分。
type Adapter interface {
	// AudioTraits 认出音频设备 id 的接口类型、是不是回录源（选设备的规则只看它们）。认不出时返回零值和原因
	// （例如 Linux 上没装 pactl）：这个设备照常可用，接口类型当未知。
	AudioTraits(ctx context.Context, id string) (AudioTraits, error)
	// Cameras 列出摄像头（拍照用的，不含元数据节点、屏幕录制）。
	Cameras(ctx context.Context) ([]terminal.Device, error)
	// CameraInput 决定怎样向摄像头要画面：captureSize 是想要的分辨率（例如 1280x720），
	// 摄像头没有这个分辨率时各系统可以改要别的（见 CameraCapture）。
	CameraInput(ctx context.Context, id, captureSize string) CameraCapture
}

// AudioTraits 是系统报的一个音频设备的属性。
type AudioTraits struct {
	Bus terminal.Bus
	// Monitor：回录源（录的是音箱放出来的声音，不是麦克风），不能当麦克风用
	Monitor bool
}

// CameraCapture 是向摄像头要画面的做法。
type CameraCapture struct {
	Input  []string // 交给 ffmpeg 的输入参数（含 -f 与 -i）
	Format string   // 向摄像头要的格式（如 mjpeg、yuyv422）；空 = 没指定，由驱动定
	Size   string   // 向摄像头要的分辨率
}
