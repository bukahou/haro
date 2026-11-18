//go:build !linux && !darwin

package osdiff

import (
	"context"

	"github.com/bukahou/haro/internal/terminal"
)

// otherAdapter：还没适配的系统（Windows 等）。照样能编译、能录音放音，只是认不出蓝牙、没有摄像头（降级，不崩）。
type otherAdapter struct{}

var _ Adapter = otherAdapter{}

// Current 返回本次编译的系统的实现。
func Current() Adapter { return otherAdapter{} }

func (otherAdapter) AudioBus(string, string) terminal.Bus               { return terminal.BusUnknown }
func (otherAdapter) Cameras(context.Context) ([]terminal.Device, error) { return nil, nil }
func (otherAdapter) CameraInput(context.Context, string, string) CameraCapture {
	return CameraCapture{}
}
