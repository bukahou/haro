//go:build !linux

package camera

import (
	"context"
	"errors"
	"log/slog"

	"github.com/bukahou/haro/internal/terminal"
)

// MediaDevicesSupported：这个系统上还没接 mediadevices（macOS 待真机验证），只能用 ffmpeg。
const MediaDevicesSupported = false

// FindDevice 见 Linux 版。
type FindDevice func(ctx context.Context) (path, name string, ok bool)

// MediaDevicesCamera 在这个系统上不可用。
type MediaDevicesCamera struct{}

// NewMediaDevicesCamera 在这个系统上总是返回错误。
func NewMediaDevicesCamera(FindDevice, string, terminal.FrameSize, int, *slog.Logger) (*MediaDevicesCamera, error) {
	return nil, errors.New("这个系统上还没接 mediadevices 拍照（HARO_CAMERA_BACKEND 改用 ffmpeg 或 auto）")
}

func (*MediaDevicesCamera) Capture(context.Context, int) (terminal.Frame, error) {
	return terminal.Frame{}, terminal.ErrNoCamera
}

func (*MediaDevicesCamera) CurrentName() string { return "" }
