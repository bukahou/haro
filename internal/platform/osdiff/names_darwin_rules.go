package osdiff

import (
	"regexp"
	"strings"

	"github.com/bukahou/haro/internal/terminal"
)

// 这个文件不带构建标签（文件名不以 _darwin.go 结尾）：macOS 的解析规则在开发机上也能测。
// ⚠️ 下面的 UID 形式与 ffmpeg 输出格式按公开资料写，要在 Mac 真机上核对（v4 第 5 步）。

// macAddressUID：蓝牙设备的 CoreAudio UID 以 MAC 地址开头，例如 00-00-5E-00-53-01:input。
var macAddressUID = regexp.MustCompile(`^([0-9A-Fa-f]{2}-){5}[0-9A-Fa-f]{2}`)

// darwinAudioBus 按 CoreAudio 设备 UID 认接口类型：
//
//	E4-76-84-…:input / :output        → 蓝牙
//	AppleUSBAudioEngine:…             → USB
//	BuiltInMicrophoneDevice / BuiltIn… → 内置
func darwinAudioBus(id string) terminal.Bus {
	switch {
	case macAddressUID.MatchString(id):
		return terminal.BusBluetooth
	case strings.HasPrefix(id, "AppleUSBAudioEngine"):
		return terminal.BusUSB
	case strings.HasPrefix(id, "BuiltIn"):
		return terminal.BusBuiltin
	default:
		return terminal.BusUnknown
	}
}

var avfoundationDevice = regexp.MustCompile(`\]\s*\[(\d+)\]\s+(.+?)\s*$`)

// parseAVFoundationCameras 解析 `ffmpeg -f avfoundation -list_devices true -i ""` 的输出（在 stderr），
// 只取视频设备、去掉屏幕录制：
//
//	[AVFoundation indev @ 0x…] AVFoundation video devices:
//	[AVFoundation indev @ 0x…] [0] FaceTime HD Camera
//	[AVFoundation indev @ 0x…] [1] Capture screen 0
//	[AVFoundation indev @ 0x…] AVFoundation audio devices:
//	[AVFoundation indev @ 0x…] [0] MacBook Pro Microphone
func parseAVFoundationCameras(output string) []terminal.Device {
	var cameras []terminal.Device
	inVideo := false
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.Contains(line, "AVFoundation video devices:"):
			inVideo = true
			continue
		case strings.Contains(line, "AVFoundation audio devices:"):
			inVideo = false
			continue
		}
		if !inVideo {
			continue
		}
		match := avfoundationDevice.FindStringSubmatch(line)
		if match == nil || strings.HasPrefix(match[2], "Capture screen") {
			continue
		}
		cameras = append(cameras, terminal.Device{ID: match[1], Name: match[2], Bus: terminal.BusUnknown})
	}
	return cameras
}

// avfoundationInput 是 macOS 上 ffmpeg 的摄像头输入参数。avfoundation 要求明确帧率（30 是 Mac 摄像头普遍支持的）。
func avfoundationInput(id, captureSize string) []string {
	return []string{"-f", "avfoundation", "-framerate", "30", "-video_size", captureSize, "-i", id}
}
