// Package camera 是平台层的拍照实现，按需拍一帧（拍的时候才打开摄像头）：
//   - MediaDevicesCamera（Linux）：pion/mediadevices 直接读摄像头，预热到曝光稳定再取帧；
//   - FFmpegCamera（备用，也是 macOS 现在用的）：ffmpeg 抓一帧，不预热。各系统的 ffmpeg 输入参数由 Locate 给出，这里不认系统。
package camera

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"image/jpeg"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/terminal"
)

// FFmpegCamera 每次调用才用 ffmpeg 抓一帧（约 0.8 秒）。抓失败后，下一次先重新找摄像头。
type FFmpegCamera struct {
	locate      Locate
	outputWidth int // 默认的最大宽度（画面更窄就原样送），高度按比例
	health      *health
	frameSize   *frameSizeLog

	mu    sync.Mutex
	input []string // 当前摄像头的输入参数；nil = 下次先重新找
}

// NewFFmpegCamera 创建按需抓帧器；name 是启动时选中的摄像头。
func NewFFmpegCamera(locate Locate, name string, outputWidth int, logger *slog.Logger) *FFmpegCamera {
	return &FFmpegCamera{locate: locate, outputWidth: outputWidth, health: newHealth(name, logger),
		frameSize: newFrameSizeLog(logger)}
}

// Capture 现抓一帧（JPEG）：画面比 maxWidth 宽就缩小（0 = 默认宽度）。
func (c *FFmpegCamera) Capture(ctx context.Context, maxWidth int) (terminal.Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := c.health.currentName()
	if c.input == nil {
		input, found, ok := c.locate(ctx)
		if !ok {
			c.health.markDown("没有找到摄像头", nil)
			return terminal.Frame{}, terminal.ErrNoCamera
		}
		c.input, name = input, found
	}
	var stdout, stderr bytes.Buffer
	args := append([]string{"-hide_banner", "-loglevel", "error"}, c.input...)
	args = append(args, "-frames:v", "1", "-vf", scaleFilter(cmp.Or(maxWidth, c.outputWidth)),
		"-f", "image2", "-c:v", "mjpeg", "pipe:1")
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		c.input = nil // 下次先重新找（可能被拔掉了，或换了设备号）
		err = fmt.Errorf("ffmpeg 抓帧: %w: %s", err, stderr.String())
		c.health.markDown("抓帧失败", err)
		return terminal.Frame{}, err
	}
	c.health.markUp(name)
	c.frameSize.observe(stdout.Bytes())
	frame := terminal.Frame{JPEG: stdout.Bytes(), TakenAt: time.Now(), Device: name}
	if config, err := jpeg.DecodeConfig(bytes.NewReader(frame.JPEG)); err == nil {
		frame.Width, frame.Height = config.Width, config.Height
	}
	return frame, nil
}

// CurrentName 是现在能用的摄像头名字；不可用时为空（看守据此告诉服务端）。
func (c *FFmpegCamera) CurrentName() string { return c.health.currentName() }
