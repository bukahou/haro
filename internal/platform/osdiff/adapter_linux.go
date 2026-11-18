//go:build linux

package osdiff

import (
	"context"
	"errors"
	"os/exec"
	"time"

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

// listFormatsTimeout：列格式只是查询（摄像头正被别的进程用着也能列），正常一瞬间就完。
const listFormatsTimeout = 3 * time.Second

// CameraInput 先用 ffmpeg 列出摄像头支持的格式与分辨率，再选一个明确要（规则见 chooseV4L2Capture）。
// ffmpeg 列完格式会以「打不开输入」退出（非 0），清单在 stderr 里，照常解析；列不出时照旧由驱动定格式。
func (linuxAdapter) CameraInput(ctx context.Context, id, captureSize string) CameraCapture {
	ctx, cancel := context.WithTimeout(ctx, listFormatsTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-f", "v4l2", "-list_formats", "all",
		"-i", id).CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return v4l2Capture(id, nil, captureSize)
	}
	return v4l2Capture(id, parseV4L2Formats(string(output)), captureSize)
}
