//go:build darwin

package osdiff

import (
	"context"
	"errors"
	"os/exec"

	"github.com/bukahou/haro/internal/terminal"
)

// darwinAdapter：音频设备 ID 是 CoreAudio 的设备 UID；摄像头走 AVFoundation（经 ffmpeg）。
type darwinAdapter struct{}

var _ Adapter = darwinAdapter{} // 少实现一个方法，Mac 版就编译不过（开发机上 make check-darwin 能查）

// Current 返回本次编译的系统的实现。
func Current() Adapter { return darwinAdapter{} }

// AudioTraits：接口类型按 CoreAudio 的设备 UID 认；CoreAudio 没有回录源。
func (darwinAdapter) AudioTraits(_ context.Context, id, _ string) (AudioTraits, error) {
	return AudioTraits{Bus: darwinAudioBus(id)}, nil
}

// Cameras 用 ffmpeg 列 AVFoundation 设备（要起一个进程，所以只在启动时列一次，P-1）。
// ffmpeg 列完设备会以「找不到输入」退出（非 0），设备清单在 stderr 里，照常解析。
func (darwinAdapter) Cameras(ctx context.Context) ([]terminal.Device, error) {
	output, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-f", "avfoundation",
		"-list_devices", "true", "-i", "").CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, err // 没装 ffmpeg 等：当作没有摄像头
	}
	return parseAVFoundationCameras(string(output)), nil
}

// CameraInput：avfoundation 要的分辨率摄像头没有时 ffmpeg 直接报错退出（不会悄悄换），所以照原样要。
func (darwinAdapter) CameraInput(_ context.Context, id, captureSize string) CameraCapture {
	return CameraCapture{Input: avfoundationInput(id, captureSize), Size: captureSize}
}
