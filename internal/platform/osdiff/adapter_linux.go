//go:build linux

package osdiff

import (
	"context"

	"github.com/bukahou/haro/internal/terminal"
)

// linuxAdapter：音频设备 ID 是 PulseAudio / PipeWire 的节点名；摄像头走 V4L2。
type linuxAdapter struct{ sysVideo string }

var _ Adapter = linuxAdapter{} // 少实现一个方法，Linux 版就编译不过

// Current 返回本次编译的系统的实现。
func Current() Adapter { return linuxAdapter{sysVideo: "/sys/class/video4linux"} }

func (linuxAdapter) AudioBus(id, _ string) terminal.Bus { return linuxAudioBus(id) }

func (a linuxAdapter) Cameras(context.Context) ([]terminal.Device, error) {
	return listV4L2Cameras(a.sysVideo)
}

func (linuxAdapter) CameraInput(id, captureSize string) []string { return v4l2Input(id, captureSize) }
