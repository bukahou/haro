// Package osdiff 是各操作系统做法不同的那部分（v4 第 5 步）。录音放音三个系统共用 miniaudio，不在这里；
// 这里只有三件事：从设备 ID 认出接口类型、列摄像头、给 ffmpeg 的摄像头输入参数。
//
// 每个系统一份实现（adapter_linux.go / adapter_darwin.go / adapter_other.go），编译时按 GOOS 自动选中一份，
// 由 Current() 返回。每份都有 `var _ Adapter = …` 编译期检查：少实现一个方法，那个系统的版本就编译不过。
//
// 本包是纯 Go（不含 cgo）：开发机上能用 `make check-darwin`（GOOS=darwin go vet）检查 Mac 版是否满足契约。
// 解析函数（names_*.go）不带构建标签，各系统的解析逻辑在任何机器上都能跑单元测试。
package osdiff

import (
	"context"

	"haro/internal/terminal"
)

// Adapter 是每个操作系统各写一份的部分。
type Adapter interface {
	// AudioBus 按系统的设备标识认出接口类型（选设备规则只看它）。
	AudioBus(id, name string) terminal.Bus
	// Cameras 列出摄像头（拍照用的，不含元数据节点、屏幕录制）。
	Cameras(ctx context.Context) ([]terminal.Device, error)
	// CameraInput 是交给 ffmpeg 的输入参数（含 -f 与 -i）。
	CameraInput(id, captureSize string) []string
}
